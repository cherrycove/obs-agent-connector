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

	dcodeconfig "github.com/GuanceCloud/obs-agent-connector/internal/adapters/dcode/config"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/transport"
)

func TestProcessQueueUploadsDcodeTurn(t *testing.T) {
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
	transcript := writeDcodeTranscript(t, "session-1", "hello", "world")
	payloads := []struct {
		event   string
		payload map[string]any
	}{
		{"UserPromptSubmit", map[string]any{"session_id": "session-1", "prompt_id": "prompt-1", "cwd": "/workspace", "transcript_path": transcript, "prompt": "hello"}},
		{"Stop", map[string]any{"session_id": "session-1", "prompt_id": "prompt-1", "cwd": "/workspace", "transcript_path": transcript, "last_assistant_message": "world"}},
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
	if err := ProcessQueue(queuePath, RunOptions{Config: &cfg, HTTPClient: server.Client()}); err != nil {
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
	transcript := writeDcodeTranscript(t, "session-2", "hello", "world")
	for _, item := range []struct {
		event   string
		payload map[string]any
	}{
		{"UserPromptSubmit", map[string]any{"session_id": "session-2", "prompt_id": "prompt-2", "cwd": "/workspace", "transcript_path": transcript, "prompt": "hello"}},
		{"Stop", map[string]any{"session_id": "session-2", "prompt_id": "prompt-2", "cwd": "/workspace", "transcript_path": transcript, "last_assistant_message": "world"}},
	} {
		if err := RecordEvent(item.event, item.payload, cfg); err != nil {
			t.Fatal(err)
		}
	}
	stop := map[string]any{"session_id": "session-2", "prompt_id": "prompt-2", "cwd": "/workspace", "transcript_path": transcript, "last_assistant_message": "world"}
	queuePath, err := enqueueStop(stop, cfg)
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{Config: &cfg, HTTPClient: server.Client()}
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

func TestPayloadForStorageHonorsCaptureModeAndRedactsSecrets(t *testing.T) {
	payload := map[string]any{
		"session_id": "session-3", "prompt_id": "prompt-3", "cwd": "/workspace",
		"transcript_path": "/tmp/transcript.jsonl", "prompt": "secret prompt",
		"tool_input": map[string]any{"X-Token": "agent_secret", "command": "go test ./..."},
	}
	disabled := payloadForStorage(payload, dcodeconfig.Config{CaptureContent: "none", MaxChars: 20_000})
	if disabled["session_id"] != "session-3" || disabled["prompt_id"] != "prompt-3" || disabled["prompt"] != nil || disabled["tool_input"] != nil {
		t.Fatalf("capture=none retained content or removed identity: %#v", disabled)
	}
	enabled := payloadForStorage(payload, dcodeconfig.Config{CaptureContent: "preview", MaxChars: 20_000})
	toolInput := enabled["tool_input"].(map[string]any)
	if toolInput["X-Token"] != "[REDACTED]" || toolInput["command"] != "go test ./..." {
		t.Fatalf("tool input was not sanitized: %#v", toolInput)
	}
}

func testConfig(t *testing.T, endpoint string) dcodeconfig.Config {
	t.Helper()
	root := t.TempDir()
	return dcodeconfig.Config{
		Enabled: true, CaptureContent: "preview", MaxChars: 20_000,
		StateDir: filepath.Join(root, "state"), LogFile: filepath.Join(root, "gtrace-hooks.json"),
		ResourceAttributes: map[string]any{"service.name": "gtrace-dcode"},
		Transport:          transport.Config{Endpoint: endpoint, TracePath: "v1/traces", MetricsPath: "v1/metrics", Timeout: time.Second},
	}
}

func writeDcodeTranscript(t *testing.T, sessionID, prompt, response string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), sessionID+".jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for index, value := range []map[string]any{
		{"schema_version": 1, "sequence": 0, "record_id": "user-1", "thread_id": sessionID, "role": "user", "message_id": "user-1", "content": prompt},
		{"schema_version": 1, "sequence": 1, "record_id": "assistant-1", "thread_id": sessionID, "role": "assistant", "message_id": "assistant-1", "content": response},
	} {
		value["sequence"] = index
		if err := encoder.Encode(value); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
