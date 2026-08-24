package hook

import (
	"bufio"
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
	"strings"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/dcode/buildinfo"
	dcodeconfig "github.com/GuanceCloud/obs-agent-connector/internal/adapters/dcode/config"
	dcodeparse "github.com/GuanceCloud/obs-agent-connector/internal/adapters/dcode/parse"
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

type queuedTurn struct {
	SessionID      string                    `json:"session_id"`
	TurnID         string                    `json:"turn_id,omitempty"`
	Cwd            string                    `json:"cwd"`
	TranscriptPath string                    `json:"transcript_path"`
	LastAssistant  string                    `json:"last_assistant_message,omitempty"`
	AgentVersion   string                    `json:"agent_version,omitempty"`
	Events         []dcodeparse.JournalEvent `json:"events,omitempty"`
	Turn           *model.Turn               `json:"turn,omitempty"`
}

type RunOptions struct {
	Config     *dcodeconfig.Config
	HTTPClient *http.Client
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
	cfg := dcodeconfig.Resolve(dcodeconfig.ResolveOptions{Cwd: cwd})
	if !cfg.Enabled {
		return 0
	}
	payload, err := readPayload(os.Stdin)
	if err != nil {
		return 0
	}
	if payloadCwd := stringValue(payload, "cwd"); payloadCwd != "" {
		cfg = dcodeconfig.Resolve(dcodeconfig.ResolveOptions{Cwd: payloadCwd})
		if !cfg.Enabled {
			return 0
		}
	}
	event := firstNonEmpty(args[0], stringValue(payload, "hook_event_name", "hookEventName"))
	if err := RecordEvent(event, payload, cfg); err != nil {
		appendLog(cfg, "hook failed", map[string]any{"event": event, "error": err.Error()})
		return 0
	}
	executable, err := os.Executable()
	if err == nil {
		if strings.EqualFold(event, "Stop") {
			_, err = enqueueStop(payload, cfg)
		}
		if err == nil {
			err = startPendingWorkers(executable, cfg)
		}
	}
	if err != nil {
		appendLog(cfg, "worker queue failed", map[string]any{"event": event, "error": err.Error()})
	}
	return 0
}

func RecordEvent(event string, payload map[string]any, cfg dcodeconfig.Config) error {
	if !cfg.Enabled {
		return nil
	}
	event = firstNonEmpty(event, stringValue(payload, "hook_event_name", "hookEventName"))
	if event == "" {
		return errors.New("dcode Hook event is required")
	}
	sessionID := stringValue(payload, "session_id", "sessionId")
	if sessionID == "" {
		return errors.New("dcode Hook payload is missing session_id")
	}
	journal := journalPath(cfg.StateDir, sessionID)
	lock, err := acquireLock(journal + ".lock")
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	if strings.EqualFold(event, "UserPromptSubmit") {
		if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(journal, nil, 0o600); err != nil {
			return err
		}
	}
	stored := dcodeparse.JournalEvent{Event: event, RecordedNano: time.Now().UnixNano(), Payload: payloadForStorage(payload, cfg)}
	if err := appendJournal(journal, stored); err != nil {
		return err
	}
	appendLog(cfg, hooklog.HookInvoked, map[string]any{"event": event, "session_id_hash": shortHash(sessionID)})
	return nil
}

func enqueueStop(payload map[string]any, cfg dcodeconfig.Config) (string, error) {
	storedPayload := payloadForStorage(payload, cfg)
	sessionID := stringValue(storedPayload, "session_id", "sessionId")
	if sessionID == "" {
		return "", errors.New("dcode Stop payload is missing session_id")
	}
	transcriptPath := stringValue(storedPayload, "transcript_path", "transcriptPath")
	if transcriptPath == "" {
		return "", errors.New("dcode Stop payload is missing transcript_path")
	}
	events, err := readJournal(journalPath(cfg.StateDir, sessionID))
	if err != nil {
		return "", err
	}
	queueDir := filepath.Join(cfg.StateDir, "queue")
	if err := os.MkdirAll(queueDir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(queueDir, "turn-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	queued := queuedTurn{
		SessionID: sessionID, TurnID: stringValue(storedPayload, "prompt_id", "promptId"),
		Cwd: stringValue(storedPayload, "cwd"), TranscriptPath: transcriptPath,
		LastAssistant: stringValue(storedPayload, "last_assistant_message", "lastAssistantMessage"),
		AgentVersion:  stringValue(storedPayload, "version", "dcode_version"), Events: events,
	}
	if err := json.NewEncoder(file).Encode(queued); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	remove = false
	return path, nil
}

func startPendingWorkers(executable string, cfg dcodeconfig.Config) error {
	queueDir := filepath.Join(cfg.StateDir, "queue")
	entries, err := os.ReadDir(queueDir)
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
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "turn-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(queueDir, entry.Name())
		if activeLock(path+".lock", 2*time.Minute) {
			continue
		}
		command := exec.Command(executable, "hook", "dcode", "worker", path)
		command.Stdin, command.Stdout, command.Stderr = devNull, devNull, devNull
		if err := command.Start(); err != nil {
			return err
		}
		_ = command.Process.Release()
	}
	return nil
}

func ProcessQueue(queuePath string, options RunOptions) error {
	lock, err := acquireLock(queuePath + ".lock")
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	queued, err := readQueue(queuePath)
	if err != nil {
		return err
	}
	cfg := dcodeconfig.Resolve(dcodeconfig.ResolveOptions{Cwd: queued.Cwd})
	if options.Config != nil {
		cfg = *options.Config
	}
	if !cfg.Enabled {
		return os.Remove(queuePath)
	}
	if queued.Turn == nil {
		turn, ok, err := dcodeparse.ReadTurn(dcodeparse.Options{
			TranscriptPath: queued.TranscriptPath, SessionID: queued.SessionID, TurnID: queued.TurnID,
			Cwd: queued.Cwd, LastAssistant: queued.LastAssistant, AgentVersion: queued.AgentVersion,
			CaptureContent: cfg.CaptureContent, MaxChars: cfg.MaxChars,
			ResourceAttributes: cfg.ResourceAttributes, Events: queued.Events,
		})
		if err != nil {
			return err
		}
		if !ok {
			return os.Remove(queuePath)
		}
		queued.Turn = &turn
		queued.Events = nil
		queued.LastAssistant = ""
		queued.TranscriptPath = ""
		if err := writeQueue(queuePath, queued); err != nil {
			return err
		}
	}
	if err := exportTurn(cfg, *queued.Turn, options.HTTPClient); err != nil {
		return err
	}
	if err := os.Remove(queuePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	appendLog(cfg, "turn uploaded", map[string]any{"session_id_hash": shortHash(queued.Turn.SessionID), "turn_id_hash": shortHash(queued.Turn.TurnID)})
	return nil
}

func exportTurn(cfg dcodeconfig.Config, turn model.Turn, httpClient *http.Client) error {
	spans := (semantic.Builder{ScopeName: "gtrace-dcode-collector", ScopeVersion: buildinfo.Version}).Build(turn)
	if len(spans) == 0 {
		return nil
	}
	manager := state.Manager{Root: filepath.Join(cfg.StateDir, "uploads"), StaleAfter: 10 * time.Minute}
	claim, err := manager.Claim(turn.SessionID, turn.TurnID, fingerprint(turn))
	if errors.Is(err, state.ErrAlreadyCompleted) || (err == nil && claim == nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim dcode turn: %w", err)
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
	builtMetrics := metrics.Build(spans)
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

func payloadForStorage(payload map[string]any, cfg dcodeconfig.Config) map[string]any {
	metadataKeys := []string{
		"hook_event_name", "hookEventName", "session_id", "sessionId", "prompt_id", "promptId", "cwd",
		"transcript_path", "transcriptPath", "tool_name", "toolName", "tool_use_id", "toolUseId",
		"agent_id", "agentId", "agent_type", "agentType", "duration_ms", "durationMs",
		"is_interrupt", "isInterrupt", "version", "dcode_version",
	}
	contentKeys := []string{"prompt", "last_assistant_message", "lastAssistantMessage", "tool_input", "toolInput", "tool_response", "toolResponse", "error"}
	out := map[string]any{}
	for _, key := range metadataKeys {
		if value := payload[key]; value != nil {
			out[key] = privacy.Sanitize(value, cfg.MaxChars)
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
	var value map[string]any
	if err := json.NewDecoder(io.LimitReader(reader, 4*1024*1024)).Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func appendJournal(path string, event dcodeparse.JournalEvent) error {
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

func readJournal(path string) ([]dcodeparse.JournalEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events := make([]dcodeparse.JournalEvent, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event dcodeparse.JournalEvent
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			events = append(events, event)
		}
	}
	return events, scanner.Err()
}

func journalPath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "journal", derivedID("dcode", sessionID)+".jsonl")
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
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	temp := path + ".tmp"
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
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

func appendLog(cfg dcodeconfig.Config, message string, extra map[string]any) {
	_ = hooklog.Append(cfg.LogFile, message, extra)
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

func stringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
