package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	grokconfig "github.com/GuanceCloud/obs-agent-connector/internal/adapters/grok/config"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/state"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/transport"
)

func TestDisabledHookExitsBeforeReadingInput(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	cfg.Enabled = false
	read := false
	err := RunHook("Stop", RunOptions{
		Config: &cfg,
		ReadInput: func() (map[string]any, error) {
			read = true
			return map[string]any{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if read {
		t.Fatal("disabled Grok Hook read stdin")
	}
}

func TestRecordEventUsesActivePromptForMissingPromptID(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	user := map[string]any{
		"hookEventName": "UserPromptSubmit", "sessionId": "session-1", "promptId": "prompt-1",
		"cwd": "/synthetic/workspace", "transcriptPath": "/synthetic/updates.jsonl", "prompt": "inspect",
	}
	if err := RecordEvent("UserPromptSubmit", user, cfg); err != nil {
		t.Fatal(err)
	}
	tool := map[string]any{
		"hookEventName": "PreToolUse", "sessionId": "session-1", "toolUseId": "tool-1",
		"toolName": "read_file", "toolInput": map[string]any{"file_path": "/synthetic/file.go"},
	}
	if err := RecordEvent("PreToolUse", tool, cfg); err != nil {
		t.Fatal(err)
	}
	events, err := readJournal(journalPath(cfg.StateDir, "session-1", "prompt-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Payload["promptId"] != "prompt-1" || events[1].Payload["transcriptPath"] != "/synthetic/updates.jsonl" {
		t.Fatalf("active turn fallback was not persisted: %#v", events)
	}
}

func TestConcurrentDuplicateHooksKeepJournalValid(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	user := map[string]any{
		"sessionId": "session-concurrent", "promptId": "prompt-concurrent",
		"transcriptPath": "/synthetic/updates.jsonl", "prompt": "inspect",
	}
	if err := RecordEvent("UserPromptSubmit", user, cfg); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"sessionId": "session-concurrent", "toolUseId": "tool-duplicate",
		"toolName": "read_file", "toolInput": map[string]any{"file_path": "/synthetic/file.go"},
	}
	const duplicates = 8
	errorsByWorker := make(chan error, duplicates)
	var workers sync.WaitGroup
	for range duplicates {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsByWorker <- RecordEvent("PreToolUse", payload, cfg)
		}()
	}
	workers.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := readJournal(journalPath(cfg.StateDir, "session-concurrent", "prompt-concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != duplicates+1 {
		t.Fatalf("concurrent duplicate Hooks corrupted or lost journal rows: got %d, want %d", len(events), duplicates+1)
	}
}

func TestBlockedStopDoesNotQueue(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	base := map[string]any{"sessionId": "session-blocked", "promptId": "prompt-blocked", "transcriptPath": "/synthetic/updates.jsonl"}
	if err := RecordEvent("UserPromptSubmit", mergePayload(base, map[string]any{"prompt": "hello"}), cfg); err != nil {
		t.Fatal(err)
	}
	blocked := mergePayload(base, map[string]any{"stopHookActive": true, "reason": "end_turn"})
	if err := RecordEvent("Stop", blocked, cfg); err != nil {
		t.Fatal(err)
	}
	if err := enqueueForEvent("Stop", blocked, cfg); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "queue"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("blocked Stop created queue entries: %#v", entries)
	}
}

func TestStopWithoutPromptIDDoesNotQueueActiveTurn(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	user := map[string]any{
		"sessionId": "session-shutdown", "promptId": "prompt-active",
		"transcriptPath": "/synthetic/updates.jsonl", "prompt": "hello",
	}
	if err := RecordEvent("UserPromptSubmit", user, cfg); err != nil {
		t.Fatal(err)
	}
	shutdown := map[string]any{"sessionId": "session-shutdown", "reason": "channel_closed", "stopHookActive": false}
	if err := enqueueForEvent("Stop", shutdown, cfg); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "queue"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("session-shutdown Stop created queue entries: %#v", entries)
	}
}

func TestProcessQueueWaitsForTurnCompletedThenUploads(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests[request.URL.Path]++
		mu.Unlock()
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)
	transcript := filepath.Join(t.TempDir(), "updates.jsonl")
	writeTranscript(t, transcript, []map[string]any{
		xaiUpdate(100, "session-pending", map[string]any{"sessionUpdate": "response_started", "message_id": "message-1", "model": "grok-code", "input_tokens": 5}),
		xaiUpdate(101, "session-pending", map[string]any{"sessionUpdate": "response_completed", "message_id": "message-1", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 5, "output_tokens": 2}}),
	})
	user := map[string]any{
		"sessionId": "session-pending", "promptId": "prompt-pending", "cwd": "/synthetic/workspace",
		"transcriptPath": transcript, "prompt": "hello",
	}
	stop := mergePayload(user, map[string]any{"reason": "end_turn", "stopHookActive": false, "lastAssistantMessage": "world"})
	if err := RecordEvent("UserPromptSubmit", user, cfg); err != nil {
		t.Fatal(err)
	}
	if err := RecordEvent("Stop", stop, cfg); err != nil {
		t.Fatal(err)
	}
	ctx, err := resolveContext("Stop", stop, cfg)
	if err != nil {
		t.Fatal(err)
	}
	queuePath, err := enqueueTurn(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{Config: &cfg, HTTPClient: server.Client(), SkipWait: true}
	if err := ProcessQueue(queuePath, options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(queuePath); err != nil {
		t.Fatalf("pending queue was removed before TurnCompleted: %v", err)
	}
	mu.Lock()
	if len(requests) != 0 {
		t.Fatalf("pending turn uploaded early: %#v", requests)
	}
	mu.Unlock()
	appendTranscript(t, transcript, xaiUpdate(102, "session-pending", map[string]any{
		"sessionUpdate": "turn_completed", "prompt_id": "prompt-pending", "stop_reason": "end_turn", "agent_result": "world",
	}))
	if err := ProcessQueue(queuePath, options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(queuePath); !os.IsNotExist(err) {
		t.Fatalf("completed queue remains: %v", err)
	}
	if _, err := os.Stat(journalPath(cfg.StateDir, "session-pending", "prompt-pending")); !os.IsNotExist(err) {
		t.Fatalf("completed turn journal remains: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/v1/traces"] != 1 || requests["/v1/metrics"] != 1 {
		t.Fatalf("unexpected upload requests: %#v", requests)
	}
}

func TestProcessQueuePreservesQueueWhileUploadClaimIsActive(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	queuePath := filepath.Join(cfg.StateDir, "queue", "turn-active-claim.json")
	now := time.Now().UnixNano()
	turn := model.Turn{
		SessionID: "session-active-claim", TurnID: "prompt-active-claim",
		AgentRuntime: "grok", AgentName: "Grok Build",
		StartUnixNano: now - int64(time.Second), EndUnixNano: now,
		FinalStatus: model.FinalStatusCompleted, InputLength: 1, OutputLength: 1,
	}
	if err := writeQueue(queuePath, queuedTurn{SessionID: turn.SessionID, TurnID: turn.TurnID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	manager := state.Manager{Root: filepath.Join(cfg.StateDir, "uploads"), StaleAfter: 10 * time.Minute}
	claim, err := manager.Claim(turn.SessionID, turn.TurnID, fingerprint(turn))
	if err != nil || claim == nil {
		t.Fatalf("create active upload claim: claim=%#v err=%v", claim, err)
	}
	t.Cleanup(func() { _ = claim.Release() })

	err = ProcessQueue(queuePath, RunOptions{Config: &cfg, SkipWait: true})
	if !errors.Is(err, errTurnClaimBusy) {
		t.Fatalf("expected active upload claim error, got %v", err)
	}
	if _, err := os.Stat(queuePath); err != nil {
		t.Fatalf("active upload claim caused queue loss: %v", err)
	}
}

func TestProcessQueueRemovesAlreadyCompletedQueueWithoutTranscript(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	queuePath := filepath.Join(cfg.StateDir, "queue", "turn-completed.json")
	queued := queuedTurn{
		SessionID: "session-completed", TurnID: "prompt-completed",
		TranscriptPath: filepath.Join(t.TempDir(), "missing-updates.jsonl"),
	}
	if err := writeQueue(queuePath, queued); err != nil {
		t.Fatal(err)
	}
	manager := state.Manager{Root: filepath.Join(cfg.StateDir, "uploads")}
	claim, err := manager.Claim(queued.SessionID, queued.TurnID, "fingerprint")
	if err != nil || claim == nil {
		t.Fatalf("claim completed turn: claim=%#v err=%v", claim, err)
	}
	if err := claim.MarkSignalUploaded("traces", map[string]any{"status": 200}); err != nil {
		t.Fatal(err)
	}
	if err := claim.Complete("traces"); err != nil {
		t.Fatal(err)
	}
	if err := ProcessQueue(queuePath, RunOptions{Config: &cfg, SkipWait: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(queuePath); !os.IsNotExist(err) {
		t.Fatalf("completed queue was not removed: %v", err)
	}
}

func TestPendingQueuePathsPrioritizeNewestTurns(t *testing.T) {
	queueDir := t.TempDir()
	oldPath := filepath.Join(queueDir, "turn-old.json")
	newPath := filepath.Join(queueDir, "turn-new.json")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	if err := os.Chtimes(oldPath, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	paths, err := pendingQueuePaths(queueDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != newPath || paths[1] != oldPath {
		t.Fatalf("pending queue order = %#v", paths)
	}
}

func TestGrokMetricsExcludeHighCardinalitySessionLabels(t *testing.T) {
	spans := []model.Span{{
		Name: "invoke_agent", DurationMs: 1000,
		StartTimeUnixNano: "1", EndTimeUnixNano: "2",
		Attributes: map[string]any{
			"gen_ai.conversation.id": "session-sensitive",
			"session_id":             "session-sensitive",
			"final_status":           "completed",
		},
		Resource: map[string]any{"agent_runtime": "grok"},
	}}
	built := buildGrokMetrics(spans)
	if len(built) != 1 || built[0].Name != "gen_ai.workflow.duration" {
		t.Fatalf("unexpected Grok metrics: %#v", built)
	}
	for _, key := range []string{"gen_ai.conversation.id", "session_id"} {
		if _, exists := built[0].Attributes[key]; exists {
			t.Fatalf("high-cardinality attribute %q leaked into Grok metric: %#v", key, built[0].Attributes)
		}
	}
}

func TestProcessQueueRetriesOnlyFailedMetrics(t *testing.T) {
	var mu sync.Mutex
	traceRequests := 0
	metricRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Path {
		case "/v1/traces":
			traceRequests++
			response.WriteHeader(http.StatusOK)
		case "/v1/metrics":
			metricRequests++
			if metricRequests == 1 {
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			response.WriteHeader(http.StatusOK)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := testConfig(t, server.URL)
	transcript := filepath.Join(t.TempDir(), "updates.jsonl")
	writeTranscript(t, transcript, []map[string]any{
		xaiUpdate(200, "session-retry", map[string]any{"sessionUpdate": "response_started", "message_id": "message-retry", "input_tokens": 4}),
		xaiUpdate(201, "session-retry", map[string]any{"sessionUpdate": "response_completed", "message_id": "message-retry", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 4, "output_tokens": 1}}),
		xaiUpdate(202, "session-retry", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-retry", "stop_reason": "end_turn", "agent_result": "done"}),
	})
	user := map[string]any{"sessionId": "session-retry", "promptId": "prompt-retry", "transcriptPath": transcript, "prompt": "retry"}
	stop := mergePayload(user, map[string]any{"reason": "end_turn", "stopHookActive": false, "lastAssistantMessage": "done"})
	for _, item := range []struct {
		event   string
		payload map[string]any
	}{{"UserPromptSubmit", user}, {"Stop", stop}} {
		if err := RecordEvent(item.event, item.payload, cfg); err != nil {
			t.Fatal(err)
		}
	}
	ctx, _ := resolveContext("Stop", stop, cfg)
	queuePath, err := enqueueTurn(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{Config: &cfg, HTTPClient: server.Client(), SkipWait: true}
	if err := ProcessQueue(queuePath, options); err == nil {
		t.Fatal("expected first metrics upload to fail")
	}
	queued, err := readQueue(queuePath)
	if err != nil || queued.Turn == nil {
		t.Fatalf("normalized turn was not persisted for retry: %#v err=%v", queued, err)
	}
	if err := ProcessQueue(queuePath, options); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if traceRequests != 1 || metricRequests != 2 {
		t.Fatalf("partial signal recovery failed: traces=%d metrics=%d", traceRequests, metricRequests)
	}
}

func TestNewPromptRecoversEarlierDurableTurn(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	transcript := filepath.Join(t.TempDir(), "updates.jsonl")
	writeTranscript(t, transcript, []map[string]any{
		xaiUpdate(300, "session-recover", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-old", "stop_reason": "end_turn", "agent_result": "old answer"}),
	})
	old := map[string]any{"sessionId": "session-recover", "promptId": "prompt-old", "transcriptPath": transcript, "prompt": "old prompt"}
	if err := RecordEvent("UserPromptSubmit", old, cfg); err != nil {
		t.Fatal(err)
	}
	newPrompt := map[string]any{"sessionId": "session-recover", "promptId": "prompt-new", "transcriptPath": transcript, "prompt": "new prompt"}
	if err := RecordEvent("UserPromptSubmit", newPrompt, cfg); err != nil {
		t.Fatal(err)
	}
	if err := enqueueForEvent("UserPromptSubmit", newPrompt, cfg); err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(cfg.StateDir, "queue", "turn-"+derivedID("session-recover", "prompt-old")+".json")
	queued, err := readQueue(queuePath)
	if err != nil {
		t.Fatalf("old durable turn was not recovered: %v", err)
	}
	if queued.TurnID != "prompt-old" || queued.TranscriptPath != transcript {
		t.Fatalf("unexpected recovered queue: %#v", queued)
	}
}

func TestIdleAndSessionEndRecoverEarlierDurableTurn(t *testing.T) {
	for _, test := range []struct {
		name    string
		event   string
		payload map[string]any
	}{
		{name: "idle prompt", event: "Notification", payload: map[string]any{"notificationType": "idle_prompt"}},
		{name: "session end", event: "SessionEnd", payload: map[string]any{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(t, "https://example.invalid")
			sessionID := "session-recover-" + strings.ReplaceAll(test.name, " ", "-")
			turnID := "prompt-recover-" + strings.ReplaceAll(test.name, " ", "-")
			transcript := filepath.Join(t.TempDir(), "updates.jsonl")
			writeTranscript(t, transcript, []map[string]any{
				xaiUpdate(310, sessionID, map[string]any{"sessionUpdate": "turn_completed", "prompt_id": turnID, "stop_reason": "end_turn", "agent_result": "recovered answer"}),
			})
			user := map[string]any{"sessionId": sessionID, "promptId": turnID, "transcriptPath": transcript, "prompt": "recover me"}
			if err := RecordEvent("UserPromptSubmit", user, cfg); err != nil {
				t.Fatal(err)
			}
			payload := mergePayload(map[string]any{"sessionId": sessionID, "transcriptPath": transcript}, test.payload)
			if err := RecordEvent(test.event, payload, cfg); err != nil {
				t.Fatal(err)
			}
			if err := enqueueForEvent(test.event, payload, cfg); err != nil {
				t.Fatal(err)
			}
			queuePath := filepath.Join(cfg.StateDir, "queue", "turn-"+derivedID(sessionID, turnID)+".json")
			queued, err := readQueue(queuePath)
			if err != nil {
				t.Fatalf("%s did not recover durable turn: %v", test.event, err)
			}
			if queued.SessionID != sessionID || queued.TurnID != turnID {
				t.Fatalf("unexpected recovered queue: %#v", queued)
			}
		})
	}
}

func TestPayloadStorageRedactsSecretsAndHonorsCaptureNone(t *testing.T) {
	payload := map[string]any{
		"sessionId": "session-secret", "promptId": "prompt-secret", "transcriptPath": "/synthetic/updates.jsonl",
		"prompt": "sensitive prompt", "toolInput": map[string]any{"Authorization": "Bearer hidden", "command": "go test ./..."},
	}
	disabled := payloadForStorage(payload, grokconfig.Config{CaptureContent: "none", MaxChars: 20_000}, "PreToolUse")
	if disabled["sessionId"] != "session-secret" || disabled["prompt"] != nil || disabled["toolInput"] != nil {
		t.Fatalf("capture=none retained content or removed identity: %#v", disabled)
	}
	enabled := payloadForStorage(payload, grokconfig.Config{CaptureContent: "preview", MaxChars: 20_000}, "PreToolUse")
	input := enabled["toolInput"].(map[string]any)
	if input["Authorization"] != "[REDACTED]" || input["command"] != "go test ./..." {
		t.Fatalf("tool input was not recursively sanitized: %#v", input)
	}
}

func TestReadPayloadEnforces128KiBLimit(t *testing.T) {
	if _, err := readPayload(strings.NewReader(`{"sessionId":"ok"}`)); err != nil {
		t.Fatalf("small payload failed: %v", err)
	}
	tooLarge := bytes.Repeat([]byte("x"), maxPayloadBytes+1)
	if _, err := readPayload(bytes.NewReader(tooLarge)); err == nil {
		t.Fatal("oversized payload was accepted")
	}
	if _, err := readPayload(strings.NewReader(`{} {}`)); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
}

func testConfig(t *testing.T, endpoint string) grokconfig.Config {
	t.Helper()
	root := t.TempDir()
	return grokconfig.Config{
		Enabled: true, CaptureContent: "preview", MaxChars: 20_000,
		StateDir: filepath.Join(root, "state"), LogFile: filepath.Join(root, "gtrace-hooks.json"),
		ResourceAttributes: map[string]any{"service.name": "gtrace-grok", "agent_runtime": "grok", "agent_name": "Grok Build"},
		Transport:          transport.Config{Endpoint: endpoint, TracePath: "v1/traces", MetricsPath: "v1/metrics", Timeout: time.Second},
	}
}

func xaiUpdate(timestamp int, sessionID string, update map[string]any) map[string]any {
	return map[string]any{
		"timestamp": timestamp, "method": "_x.ai/session/update",
		"params": map[string]any{"sessionId": sessionID, "update": update},
	}
}

func writeTranscript(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendTranscript(t *testing.T, path string, record map[string]any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mergePayload(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}
