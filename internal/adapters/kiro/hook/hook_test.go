package hook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	kiroconfig "github.com/GuanceCloud/obs-agent-connector/internal/adapters/kiro/config"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/transport"
)

func TestProcessQueueUploadsKiroTurn(t *testing.T) {
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
	writeSession(t, cfg.SessionDir, "session-1", "/workspace", "hello", "world")
	payloads := []struct {
		event   string
		payload map[string]any
	}{
		{"UserPromptSubmit", map[string]any{"session_id": "session-1", "cwd": "/workspace", "prompt": "hello"}},
		{"Stop", map[string]any{"session_id": "session-1", "cwd": "/workspace", "assistant_response": "world"}},
	}
	for _, item := range payloads {
		if err := RecordEvent(item.event, item.payload, cfg); err != nil {
			t.Fatal(err)
		}
	}
	queuePath, err := enqueueStop(payloads[1].payload, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProcessQueue(queuePath, RunOptions{Config: &cfg, HTTPClient: server.Client(), SkipWait: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/v1/traces"] != 1 || requests["/v1/metrics"] != 1 {
		t.Fatalf("unexpected upload requests: %#v", requests)
	}
	if _, err := os.Stat(queuePath); !os.IsNotExist(err) {
		t.Fatalf("completed queue file remains: %v", err)
	}
}

func TestProcessQueueRetriesOnlyFailedMetricsSignal(t *testing.T) {
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
	writeSession(t, cfg.SessionDir, "session-2", "/workspace", "hello", "world")
	for _, item := range []struct {
		event   string
		payload map[string]any
	}{
		{"UserPromptSubmit", map[string]any{"session_id": "session-2", "cwd": "/workspace", "prompt": "hello"}},
		{"Stop", map[string]any{"session_id": "session-2", "cwd": "/workspace", "assistant_response": "world"}},
	} {
		if err := RecordEvent(item.event, item.payload, cfg); err != nil {
			t.Fatal(err)
		}
	}
	queuePath, err := enqueueStop(map[string]any{"session_id": "session-2", "cwd": "/workspace", "assistant_response": "world"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{Config: &cfg, HTTPClient: server.Client(), SkipWait: true}
	if err := ProcessQueue(queuePath, options); err == nil {
		t.Fatal("expected the first metrics upload to fail")
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

func TestProcessQueueUploadsModernKiroTurn(t *testing.T) {
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
	writeModernHookSession(t, cfg.SessionDir, "sess_modern", "hello", "world")
	payloads := []struct {
		event   string
		payload map[string]any
	}{
		{"UserPromptSubmit", map[string]any{"session_id": "sess_modern", "cwd": "/workspace", "prompt": "hello"}},
		{"Stop", map[string]any{"session_id": "sess_modern", "cwd": "/workspace"}},
	}
	for _, item := range payloads {
		if err := RecordEvent(item.event, item.payload, cfg); err != nil {
			t.Fatal(err)
		}
	}
	queuePath, err := enqueueStop(payloads[1].payload, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProcessQueue(queuePath, RunOptions{Config: &cfg, HTTPClient: server.Client(), SkipWait: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/v1/traces"] != 1 || requests["/v1/metrics"] != 1 {
		t.Fatalf("unexpected modern Kiro upload requests: %#v", requests)
	}
}

func TestPayloadForStorageHonorsCaptureModeAndRedactsSecrets(t *testing.T) {
	payload := map[string]any{
		"session_id": "session-3", "cwd": "/workspace", "prompt": "secret prompt",
		"tool_input": map[string]any{"X-Token": "agent_secret", "command": "go test ./..."},
	}
	disabled := payloadForStorage(payload, kiroconfig.Config{CaptureContent: "none", MaxChars: 20_000})
	if disabled["session_id"] != "session-3" || disabled["prompt"] != nil || disabled["tool_input"] != nil {
		t.Fatalf("capture=none retained content or removed identity: %#v", disabled)
	}
	enabled := payloadForStorage(payload, kiroconfig.Config{CaptureContent: "preview", MaxChars: 20_000})
	toolInput := enabled["tool_input"].(map[string]any)
	if toolInput["X-Token"] != "[REDACTED]" || toolInput["command"] != "go test ./..." {
		t.Fatalf("tool input was not sanitized: %#v", toolInput)
	}
}

func testConfig(t *testing.T, endpoint string) kiroconfig.Config {
	t.Helper()
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return kiroconfig.Config{
		Enabled: true, CaptureContent: "preview", MaxChars: 20_000,
		StateDir: filepath.Join(root, "state"), SessionDir: sessionDir, LogFile: filepath.Join(root, "gtrace-hooks.json"),
		ResourceAttributes: map[string]any{"service.name": "gtrace-kiro"},
		Transport:          transport.Config{Endpoint: endpoint, TracePath: "v1/traces", MetricsPath: "v1/metrics", Timeout: time.Second},
	}
}

func writeSession(t *testing.T, sessionDir, sessionID, cwd, prompt, response string) {
	t.Helper()
	end := time.Now().UTC()
	sidecar := map[string]any{
		"session_id": sessionID, "cwd": cwd, "updated_at": end.Format(time.RFC3339Nano),
		"session_state": map[string]any{
			"rts_model_state": map[string]any{"model_info": map[string]any{"model_id": "claude-sonnet-4"}},
			"conversation_metadata": map[string]any{"user_turn_metadatas": []any{map[string]any{
				"end_reason": "UserTurnEnd", "end_timestamp": end.Format(time.RFC3339Nano), "turn_duration": map[string]any{"secs": float64(1)},
				"input_token_count": float64(4), "output_token_count": float64(2), "message_ids": []any{"message-1"},
			}}},
		},
	}
	writeJSONFile(t, filepath.Join(sessionDir, sessionID+".json"), sidecar)
	file, err := os.Create(filepath.Join(sessionDir, sessionID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, value := range []map[string]any{
		{"kind": "Prompt", "data": map[string]any{"content": []any{map[string]any{"kind": "text", "data": prompt}}}},
		{"kind": "AssistantMessage", "data": map[string]any{"message_id": "message-1", "content": []any{map[string]any{"kind": "text", "data": response}}}},
	} {
		if err := encoder.Encode(value); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeModernHookSession(t *testing.T, sessionRoot, sessionID, prompt, response string) {
	t.Helper()
	directory := filepath.Join(sessionRoot, "workspace-hash", sessionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-3 * time.Second)
	writeJSONFile(t, filepath.Join(directory, "session.json"), map[string]any{
		"id": sessionID, "modelId": "auto", "status": "idle", "workspacePaths": []any{"/workspace"},
	})
	file, err := os.Create(filepath.Join(directory, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	records := []map[string]any{
		{"id": "user", "timestamp": start.Format(time.RFC3339Nano), "payload": map[string]any{"type": "user", "content": prompt}},
		{"id": "start", "timestamp": start.Add(time.Second).Format(time.RFC3339Nano), "payload": map[string]any{"type": "turn_start", "executionId": "execution-modern"}},
		{"id": "say", "timestamp": start.Add(2 * time.Second).Format(time.RFC3339Nano), "payload": map[string]any{"type": "assistant", "executionId": "execution-modern", "operationType": "Say", "content": response}},
		{"id": "usage", "timestamp": start.Add(2500 * time.Millisecond).Format(time.RFC3339Nano), "payload": map[string]any{"type": "usage_summary", "executionId": "execution-modern", "status": "success", "requestIds": []any{"request-modern"}}},
		{"id": "end", "timestamp": start.Add(3 * time.Second).Format(time.RFC3339Nano), "payload": map[string]any{"type": "turn_end", "executionId": "execution-modern", "stopReason": "end_turn"}},
	}
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

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
