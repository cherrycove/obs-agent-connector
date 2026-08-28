package hook

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/grok/buildinfo"
	grokconfig "github.com/GuanceCloud/obs-agent-connector/internal/adapters/grok/config"
	grokparse "github.com/GuanceCloud/obs-agent-connector/internal/adapters/grok/parse"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/hooklog"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/metrics"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/otlp"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/privacy"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/proto"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/semantic"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/state"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/transport"
)

const (
	maxPayloadBytes       = 128 * 1024
	maxWorkersPerHook     = 4
	maxQueueChecksPerHook = 32
)

var currentGOOS = runtime.GOOS
var errTurnClaimBusy = errors.New("Grok turn upload is already in progress")

type turnContext struct {
	SessionID      string `json:"session_id"`
	TurnID         string `json:"turn_id"`
	Cwd            string `json:"cwd,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	AgentVersion   string `json:"agent_version,omitempty"`
}

type queuedTurn struct {
	SessionID      string                   `json:"session_id"`
	TurnID         string                   `json:"turn_id"`
	Cwd            string                   `json:"cwd,omitempty"`
	TranscriptPath string                   `json:"transcript_path,omitempty"`
	AgentVersion   string                   `json:"agent_version,omitempty"`
	Events         []grokparse.JournalEvent `json:"events,omitempty"`
	Turn           *model.Turn              `json:"turn,omitempty"`
}

type RunOptions struct {
	Config     *grokconfig.Config
	ReadInput  func() (map[string]any, error)
	HTTPClient *http.Client
	SkipWait   bool
}

func RunCLI(args []string) int {
	if len(args) == 2 && args[0] == "worker" {
		if err := ProcessQueue(args[1], RunOptions{}); err != nil {
			return 1
		}
		return 0
	}
	if len(args) != 1 {
		return 0
	}

	cwd, _ := os.Getwd()
	cfg := grokconfig.Resolve(grokconfig.ResolveOptions{Cwd: cwd})
	if err := RunHook(args[0], RunOptions{Config: &cfg}); err != nil {
		appendLog(cfg, "hook failed", map[string]any{"event": canonicalEvent(args[0]), "error": err.Error()})
	}
	return 0
}

func RunHook(event string, options RunOptions) error {
	cfg := grokconfig.Resolve(grokconfig.ResolveOptions{})
	if options.Config != nil {
		cfg = *options.Config
	}
	// Grok Hooks are synchronous. Resolve the user-managed switch before
	// touching stdin or state so disabled collection is a true no-op.
	if !cfg.Enabled {
		return nil
	}
	readInput := options.ReadInput
	if readInput == nil {
		readInput = func() (map[string]any, error) { return readPayload(os.Stdin) }
	}
	payload, err := readInput()
	if err != nil {
		return err
	}
	event = canonicalEvent(firstNonEmpty(event, stringValue(payload, "hookEventName", "hook_event_name")))
	if err := RecordEvent(event, payload, cfg); err != nil {
		return err
	}
	if err := enqueueForEvent(event, payload, cfg); err != nil {
		return fmt.Errorf("queue Grok worker: %w", err)
	}
	if executable, executableErr := os.Executable(); executableErr == nil {
		if workerErr := startPendingWorkers(executable, cfg); workerErr != nil {
			return fmt.Errorf("start Grok worker: %w", workerErr)
		}
	}
	if strings.EqualFold(event, "SessionEnd") {
		if sessionID := stringValue(payload, "sessionId", "session_id"); sessionID != "" {
			_ = os.Remove(activePath(cfg.StateDir, sessionID))
		}
	}
	return nil
}

func RecordEvent(event string, payload map[string]any, cfg grokconfig.Config) error {
	if !cfg.Enabled {
		return nil
	}
	event = canonicalEvent(firstNonEmpty(event, stringValue(payload, "hookEventName", "hook_event_name")))
	if event == "" {
		return errors.New("Grok Hook event is required")
	}
	ctx, err := resolveContext(event, payload, cfg)
	if err != nil {
		return err
	}
	if ctx.TurnID == "" {
		appendLog(cfg, hooklog.HookInvoked, map[string]any{"event": event, "session_id_hash": shortHash(ctx.SessionID)})
		return nil
	}
	storedPayload := payloadForStorage(payload, cfg, event)
	storedPayload["sessionId"] = ctx.SessionID
	storedPayload["promptId"] = ctx.TurnID
	if ctx.Cwd != "" {
		storedPayload["cwd"] = ctx.Cwd
	}
	if ctx.TranscriptPath != "" {
		storedPayload["transcriptPath"] = ctx.TranscriptPath
	}

	journal := journalPath(cfg.StateDir, ctx.SessionID, ctx.TurnID)
	lock, err := acquireLock(journal + ".lock")
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	stored := grokparse.JournalEvent{Event: event, RecordedNano: time.Now().UnixNano(), Payload: storedPayload}
	if err := appendJournal(journal, stored); err != nil {
		return err
	}
	if strings.EqualFold(event, "UserPromptSubmit") {
		if err := writeJSONAtomic(activePath(cfg.StateDir, ctx.SessionID), ctx); err != nil {
			return err
		}
	}
	appendLog(cfg, hooklog.HookInvoked, map[string]any{
		"event": event, "session_id_hash": shortHash(ctx.SessionID), "turn_id_hash": shortHash(ctx.TurnID),
	})
	return nil
}

func enqueueForEvent(event string, payload map[string]any, cfg grokconfig.Config) error {
	if strings.EqualFold(event, "Stop") && stringValue(payload, "promptId", "prompt_id") == "" {
		// Grok can emit a session-shutdown Stop without a prompt ID. Do not
		// reinterpret it as the active turn's terminal signal.
		return nil
	}
	ctx, err := resolveContext(event, payload, cfg)
	if err != nil {
		return err
	}
	switch strings.ToLower(event) {
	case "stop":
		if ctx.TurnID == "" || boolValue(firstNonNil(payload["stopHookActive"], payload["stop_hook_active"])) {
			return nil
		}
		_, err = enqueueTurn(ctx, cfg)
		return err
	case "stopfailure", "stopcancelled":
		if ctx.TurnID == "" {
			return nil
		}
		_, err = enqueueTurn(ctx, cfg)
		return err
	case "userpromptsubmit", "sessionend":
		return enqueueRecoveredTurns(ctx, cfg)
	case "notification":
		if strings.EqualFold(stringValue(payload, "notificationType", "notification_type"), "idle_prompt") {
			return enqueueRecoveredTurns(ctx, cfg)
		}
	}
	return nil
}

func enqueueRecoveredTurns(ctx turnContext, cfg grokconfig.Config) error {
	if ctx.SessionID == "" || ctx.TranscriptPath == "" {
		return nil
	}
	turnIDs, err := grokparse.CompletedTurnIDs(ctx.TranscriptPath, ctx.SessionID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	manager := state.Manager{Root: filepath.Join(cfg.StateDir, "uploads")}
	for _, turnID := range turnIDs {
		completed, completedErr := manager.Completed(ctx.SessionID, turnID)
		if completedErr != nil {
			return completedErr
		}
		if completed {
			continue
		}
		if _, statErr := os.Stat(journalPath(cfg.StateDir, ctx.SessionID, turnID)); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return statErr
		}
		recovered := ctx
		recovered.TurnID = turnID
		if _, err := enqueueTurn(recovered, cfg); err != nil {
			return err
		}
	}
	return nil
}

func enqueueTurn(ctx turnContext, cfg grokconfig.Config) (string, error) {
	if ctx.SessionID == "" || ctx.TurnID == "" {
		return "", errors.New("Grok queue requires sessionId and promptId")
	}
	events, err := readJournal(journalPath(cfg.StateDir, ctx.SessionID, ctx.TurnID))
	if err != nil {
		return "", err
	}
	queueDir := filepath.Join(cfg.StateDir, "queue")
	if err := os.MkdirAll(queueDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(queueDir, "turn-"+derivedID(ctx.SessionID, ctx.TurnID)+".json")
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return "", err
	}
	defer releaseLock(lock)
	queued := queuedTurn{
		SessionID: ctx.SessionID, TurnID: ctx.TurnID, Cwd: ctx.Cwd,
		TranscriptPath: ctx.TranscriptPath, AgentVersion: ctx.AgentVersion, Events: events,
	}
	if existing, readErr := readQueue(path); readErr == nil {
		if existing.Turn != nil {
			return path, nil
		}
		if queued.Cwd == "" {
			queued.Cwd = existing.Cwd
		}
		if queued.TranscriptPath == "" {
			queued.TranscriptPath = existing.TranscriptPath
		}
		if queued.AgentVersion == "" {
			queued.AgentVersion = existing.AgentVersion
		}
	}
	if err := writeQueue(path, queued); err != nil {
		return "", err
	}
	return path, nil
}

func startPendingWorkers(executable string, cfg grokconfig.Config) error {
	queueDir := filepath.Join(cfg.StateDir, "queue")
	paths, err := pendingQueuePaths(queueDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	started := 0
	checked := 0
	for _, path := range paths {
		checked++
		if checked > maxQueueChecksPerHook || started >= maxWorkersPerHook {
			break
		}
		if activeLock(path+".lock", 2*time.Minute) {
			continue
		}
		command := exec.Command(executable, "hook", "grok", "worker", path)
		command.Stdin, command.Stdout, command.Stderr = devNull, devNull, devNull
		if err := command.Start(); err != nil {
			return err
		}
		_ = command.Process.Release()
		started++
	}
	return nil
}

type pendingQueue struct {
	path     string
	modified time.Time
}

func pendingQueuePaths(queueDir string) ([]string, error) {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return nil, err
	}
	queues := make([]pendingQueue, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "turn-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, infoErr
		}
		queues = append(queues, pendingQueue{
			path:     filepath.Join(queueDir, entry.Name()),
			modified: info.ModTime(),
		})
	}
	sort.SliceStable(queues, func(i, j int) bool {
		return queues[i].modified.After(queues[j].modified)
	})
	paths := make([]string, 0, len(queues))
	for _, queue := range queues {
		paths = append(paths, queue.path)
	}
	return paths, nil
}

func ProcessQueue(queuePath string, options RunOptions) error {
	// Like the foreground Hook, the worker checks enabled before reading state.
	cfg := grokconfig.Resolve(grokconfig.ResolveOptions{})
	if options.Config != nil {
		cfg = *options.Config
	}
	if !cfg.Enabled {
		return nil
	}
	lock, err := acquireLock(queuePath + ".lock")
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	queued, err := readQueue(queuePath)
	if err != nil {
		return err
	}
	manager := state.Manager{Root: filepath.Join(cfg.StateDir, "uploads"), StaleAfter: 10 * time.Minute}
	completed, err := manager.Completed(queued.SessionID, queued.TurnID)
	if err != nil {
		return err
	}
	if completed {
		return cleanupQueue(cfg, queuePath, queued.SessionID, queued.TurnID)
	}
	if queued.AgentVersion == "" {
		queued.AgentVersion = resourceString(cfg.ResourceAttributes, "agent_version")
	}
	if queued.Turn == nil {
		turn, ok, err := waitForTurn(queued, cfg, options.SkipWait)
		if err != nil {
			return err
		}
		if !ok {
			// A normal Stop can be blocked by another Hook. Keep the queue until
			// TurnCompleted is durable or a later explicit terminal arrives.
			return nil
		}
		queued.Turn = &turn
		queued.Events = nil
		queued.TranscriptPath = ""
		if err := writeQueue(queuePath, queued); err != nil {
			return err
		}
	}
	if err := exportTurn(cfg, *queued.Turn, options.HTTPClient); err != nil {
		return err
	}
	if err := cleanupQueue(cfg, queuePath, queued.Turn.SessionID, queued.Turn.TurnID); err != nil {
		return err
	}
	appendLog(cfg, "turn uploaded", map[string]any{
		"session_id_hash": shortHash(queued.Turn.SessionID), "turn_id_hash": shortHash(queued.Turn.TurnID),
	})
	return nil
}

func cleanupQueue(cfg grokconfig.Config, queuePath, sessionID, turnID string) error {
	journal := journalPath(cfg.StateDir, sessionID, turnID)
	if err := os.Remove(journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(queuePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func waitForTurn(queued queuedTurn, cfg grokconfig.Config, skipWait bool) (model.Turn, bool, error) {
	deadline := time.Now().Add(2 * time.Second)
	if !skipWait && strings.TrimSpace(queued.TranscriptPath) != "" {
		waitForTranscriptStable(queued.TranscriptPath, deadline)
	}
	var lastErr error
	for {
		turn, ok, err := grokparse.ReadTurn(grokparse.Options{
			TranscriptPath: queued.TranscriptPath, SessionID: queued.SessionID, TurnID: queued.TurnID,
			Cwd: queued.Cwd, AgentVersion: queued.AgentVersion,
			CaptureContent: cfg.CaptureContent, MaxChars: cfg.MaxChars,
			ResourceAttributes: cfg.ResourceAttributes, Events: queued.Events,
		})
		if err == nil && ok {
			return turn, true, nil
		}
		if err != nil {
			lastErr = err
		}
		if skipWait || time.Now().After(deadline) {
			if lastErr != nil && !errors.Is(lastErr, os.ErrNotExist) {
				return model.Turn{}, false, lastErr
			}
			return model.Turn{}, false, nil
		}
		time.Sleep(125 * time.Millisecond)
	}
}

func waitForTranscriptStable(path string, deadline time.Time) {
	var lastSize int64 = -1
	var lastModified int64 = -1
	stableSamples := 0
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil {
			modified := info.ModTime().UnixNano()
			if info.Size() == lastSize && modified == lastModified {
				stableSamples++
				if stableSamples >= 2 {
					return
				}
			} else {
				lastSize = info.Size()
				lastModified = modified
				stableSamples = 0
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		pause := 100 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}
}

func exportTurn(cfg grokconfig.Config, turn model.Turn, httpClient *http.Client) error {
	spans := (semantic.Builder{ScopeName: "gtrace-grok-collector", ScopeVersion: buildinfo.Version}).Build(turn)
	if len(spans) == 0 {
		return nil
	}
	manager := state.Manager{Root: filepath.Join(cfg.StateDir, "uploads"), StaleAfter: 10 * time.Minute}
	claim, err := manager.Claim(turn.SessionID, turn.TurnID, fingerprint(turn))
	if errors.Is(err, state.ErrAlreadyCompleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim Grok turn: %w", err)
	}
	if claim == nil {
		return errTurnClaimBusy
	}
	completed := false
	defer func() {
		if !completed {
			_ = claim.Release()
		}
	}()
	uploader := transport.Client{Config: cfg.Transport, HTTPClient: httpClient}
	if !claim.SignalWasUploaded("traces") {
		payload := proto.EncodeExportTraceServiceRequest(otlp.SpansToProtoRequest(spans))
		result, uploadErr := uploader.Upload("traces", payload)
		if uploadErr != nil {
			return uploadErr
		}
		if err := claim.MarkSignalUploaded("traces", map[string]any{"status": result.StatusCode, "bytes": len(payload)}); err != nil {
			return err
		}
		appendLog(cfg, hooklog.UploadedSpans, map[string]any{"status": result.StatusCode, "spans": len(spans)})
	}
	required := []string{"traces"}
	builtMetrics := buildGrokMetrics(spans)
	hasMetricsEndpoint := cfg.Transport.MetricsURL != "" || cfg.Transport.Endpoint != ""
	if len(builtMetrics) > 0 && hasMetricsEndpoint {
		required = append(required, "metrics")
		if !claim.SignalWasUploaded("metrics") {
			payload := proto.EncodeExportMetricsServiceRequest(otlp.MetricsToProtoRequest(builtMetrics))
			result, uploadErr := uploader.Upload("metrics", payload)
			if uploadErr != nil {
				return uploadErr
			}
			if err := claim.MarkSignalUploaded("metrics", map[string]any{"status": result.StatusCode, "bytes": len(payload)}); err != nil {
				return err
			}
			appendLog(cfg, hooklog.UploadedMetrics, map[string]any{"status": result.StatusCode, "metrics": len(builtMetrics)})
		}
	}
	if err := claim.Complete(required...); err != nil {
		return err
	}
	completed = true
	return nil
}

func buildGrokMetrics(spans []model.Span) []model.Metric {
	built := metrics.Build(spans)
	for index := range built {
		delete(built[index].Attributes, "gen_ai.conversation.id")
		delete(built[index].Attributes, "session_id")
	}
	return built
}

func resolveContext(event string, payload map[string]any, cfg grokconfig.Config) (turnContext, error) {
	ctx := turnContext{
		SessionID: stringValue(payload, "sessionId", "session_id"),
		TurnID:    stringValue(payload, "promptId", "prompt_id"),
		Cwd:       stringValue(payload, "cwd"), TranscriptPath: stringValue(payload, "transcriptPath", "transcript_path"),
		AgentVersion: stringValue(payload, "agentVersion", "grokVersion", "version"),
	}
	if ctx.SessionID == "" {
		return turnContext{}, errors.New("Grok Hook payload is missing sessionId")
	}
	active, _ := readActive(activePath(cfg.StateDir, ctx.SessionID))
	if ctx.TurnID == "" && !strings.EqualFold(event, "SessionStart") {
		ctx.TurnID = active.TurnID
	}
	if ctx.Cwd == "" {
		ctx.Cwd = active.Cwd
	}
	if ctx.TranscriptPath == "" {
		ctx.TranscriptPath = active.TranscriptPath
	}
	if ctx.AgentVersion == "" {
		ctx.AgentVersion = active.AgentVersion
	}
	if strings.EqualFold(event, "UserPromptSubmit") && ctx.TurnID == "" {
		return turnContext{}, errors.New("Grok UserPromptSubmit payload is missing promptId")
	}
	return ctx, nil
}

func payloadForStorage(payload map[string]any, cfg grokconfig.Config, event string) map[string]any {
	metadataKeys := []string{
		"hookEventName", "hook_event_name", "sessionId", "session_id", "promptId", "prompt_id", "cwd",
		"workspaceRoot", "workspace_root", "timestamp", "transcriptPath", "transcript_path", "clientIdentifier",
		"permissionMode", "modelId", "source", "reason", "stopHookActive", "backgroundTasks", "sessionCrons",
		"toolName", "toolUseId", "toolInputTruncated", "toolResultTruncated", "durationMs", "isBackgrounded",
		"notificationType", "level", "subagentId", "subagentType", "phase", "cancelledBy", "cancelTrigger",
	}
	contentKeys := []string{
		"prompt", "lastAssistantMessage", "toolInput", "toolResult", "errorDetails", "reasonDetails",
		"message", "title", "description",
	}
	out := map[string]any{}
	for _, key := range metadataKeys {
		if value := payload[key]; value != nil {
			out[key] = privacy.Sanitize(value, cfg.MaxChars)
		}
	}
	if strings.EqualFold(event, "StopFailure") {
		if value := payload["error"]; value != nil {
			out["error"] = privacy.Sanitize(value, 128)
		}
	} else if cfg.CaptureContent != "none" {
		if value := payload["error"]; value != nil {
			out["error"] = privacy.Sanitize(value, cfg.MaxChars)
		}
	}
	if cfg.CaptureContent != "none" {
		for _, key := range contentKeys {
			if value := payload[key]; value != nil {
				out[key] = privacy.Sanitize(value, cfg.MaxChars)
			}
		}
	}
	return out
}

func readPayload(reader io.Reader) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPayloadBytes {
		return nil, fmt.Errorf("Grok Hook payload exceeds %d bytes", maxPayloadBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("Grok Hook payload contains multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func appendJournal(path string, event grokparse.JournalEvent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(file).Encode(event)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func readJournal(path string) ([]grokparse.JournalEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events := make([]grokparse.JournalEvent, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var event grokparse.JournalEvent
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			events = append(events, event)
		}
	}
	return events, scanner.Err()
}

func readQueue(path string) (queuedTurn, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return queuedTurn{}, err
	}
	var value queuedTurn
	err = json.Unmarshal(body, &value)
	return value, err
}

func writeQueue(path string, value queuedTurn) error {
	return writeJSONAtomic(path, value)
}

func readActive(path string) (turnContext, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return turnContext{}, err
	}
	var value turnContext
	err = json.Unmarshal(body, &value)
	return value, err
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".grok-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := replaceJSONFile(tempPath, path); err != nil {
		return err
	}
	remove = false
	return nil
}

func replaceJSONFile(tempPath, path string) error {
	if currentGOOS != "windows" {
		return os.Rename(tempPath, path)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return os.Rename(tempPath, path)
	} else if err != nil {
		return err
	}
	backup, err := os.CreateTemp(filepath.Dir(path), ".grok-backup-*.json")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := os.Rename(path, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Rename(backupPath, path)
		return err
	}
	return os.Remove(backupPath)
}

func journalPath(stateDir, sessionID, turnID string) string {
	return filepath.Join(stateDir, "journal", derivedID("grok-session", sessionID), derivedID("grok-turn", turnID)+".jsonl")
}

func activePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "active", derivedID("grok-session", sessionID)+".json")
}

func acquireLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Second)
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			return file, nil
		}
		if errors.Is(err, os.ErrExist) && !activeLock(path, 2*time.Minute) {
			continue
		}
		if !errors.Is(err, os.ErrExist) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func activeLock(path string, staleAfter time.Duration) bool {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	if staleAfter > 0 && time.Since(info.ModTime()) > staleAfter {
		removeErr := os.Remove(path)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			return false
		}
	}
	return true
}

func releaseLock(file *os.File) {
	if file == nil {
		return
	}
	path := file.Name()
	_ = file.Close()
	_ = os.Remove(path)
}

func appendLog(cfg grokconfig.Config, message string, extra map[string]any) {
	_ = hooklog.Append(cfg.LogFile, message, extra)
}

func canonicalEvent(value string) string {
	normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case "sessionstart":
		return "SessionStart"
	case "userpromptsubmit", "beforesubmitprompt":
		return "UserPromptSubmit"
	case "pretooluse", "beforeshellexecution", "beforemcpexecution", "beforereadfile":
		return "PreToolUse"
	case "posttooluse", "aftershellexecution", "aftermcpexecution", "afterfileedit", "afteragentresponse", "afteragentthought":
		return "PostToolUse"
	case "posttoolusefailure":
		return "PostToolUseFailure"
	case "permissiondenied":
		return "PermissionDenied"
	case "stop":
		return "Stop"
	case "stopfailure":
		return "StopFailure"
	case "stopcancelled":
		return "StopCancelled"
	case "notification":
		return "Notification"
	case "subagentstart":
		return "SubagentStart"
	case "subagentstop", "subagentend":
		return "SubagentStop"
	case "precompact":
		return "PreCompact"
	case "postcompact":
		return "PostCompact"
	case "sessionend":
		return "SessionEnd"
	}
	return strings.TrimSpace(value)
}

func fingerprint(value model.Turn) string {
	body, _ := json.Marshal(value)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func derivedID(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func resourceString(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func stringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func boolValue(value any) bool {
	switch current := value.(type) {
	case bool:
		return current
	case string:
		parsed, _ := strconv.ParseBool(strings.TrimSpace(current))
		return parsed
	case float64:
		return current != 0
	case json.Number:
		parsed, _ := strconv.ParseFloat(current.String(), 64)
		return parsed != 0
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
