package hook

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cursorconfig "github.com/GuanceCloud/obs-agent-connector/internal/adapters/cursor/config"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/transport"
)

func TestBuildTurnAssemblesCursorPromptResponseAndTools(t *testing.T) {
	base := time.Date(2026, time.August, 17, 10, 0, 0, 0, time.UTC).UnixNano()
	events := []storedEvent{
		{Event: "beforeSubmitPrompt", RecordedNano: base, Payload: map[string]any{"conversation_id": "conv-1", "generation_id": "turn-1", "prompt": "inspect token=secret", "model": "claude-4.6-sonnet", "composer_mode": "agent"}},
		{Event: "preToolUse", RecordedNano: base + int64(time.Second), Payload: map[string]any{"conversation_id": "conv-1", "tool_use_id": "tool-1\nextra", "tool_name": "Shell", "tool_input": map[string]any{"command": "go test ./..."}}},
		{Event: "postToolUse", RecordedNano: base + 2*int64(time.Second), Payload: map[string]any{"conversation_id": "conv-1", "tool_use_id": "tool-1", "tool_name": "Shell", "tool_output": "ok"}},
		{Event: "afterAgentResponse", RecordedNano: base + 3*int64(time.Second), Payload: map[string]any{"conversation_id": "conv-1", "text": "done", "model": "claude-4.6-sonnet", "input_tokens": float64(12), "output_tokens": float64(4)}},
		{Event: "stop", RecordedNano: base + 4*int64(time.Second), Payload: map[string]any{"conversation_id": "conv-1", "status": "completed"}},
	}
	turn := buildTurn(events, cursorconfig.Config{CaptureContent: "preview", MaxChars: 20_000, ResourceAttributes: map[string]any{"team": "platform"}})
	if turn.SessionID != "conv-1" || turn.TurnID != "turn-1" || turn.FinalStatus != model.FinalStatusCompleted {
		t.Fatalf("unexpected turn identity: %#v", turn)
	}
	if turn.InputPreview != "inspect token=[REDACTED]" || turn.OutputPreview != "done" {
		t.Fatalf("unexpected previews: %#v", turn)
	}
	if len(turn.LLMCalls) != 1 || turn.LLMCalls[0].Provider != "anthropic" || turn.Usage.InputTokens != 12 || turn.Usage.OutputTokens != 4 {
		t.Fatalf("unexpected LLM call: %#v", turn.LLMCalls)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].CallID != "tool-1" || turn.ToolCalls[0].Command != "go test ./..." {
		t.Fatalf("unexpected tool call: %#v", turn.ToolCalls)
	}
}

func TestProcessEventUploadsTerminalCursorTurnAndClearsJournal(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests[request.URL.Path]++
		mu.Unlock()
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	stateDir := t.TempDir()
	cfg := cursorconfig.Config{
		Enabled: true, CaptureContent: "preview", MaxChars: 20_000,
		StateDir: stateDir, LogFile: filepath.Join(stateDir, "hook.log"),
		ResourceAttributes: map[string]any{"service.name": "gtrace-cursor"},
		Transport:          transport.Config{Endpoint: server.URL, TracePath: "v1/traces", MetricsPath: "v1/metrics", Timeout: time.Second},
	}
	options := RunOptions{Config: &cfg, HTTPClient: server.Client()}
	for _, item := range []struct {
		event   string
		payload map[string]any
	}{
		{"beforeSubmitPrompt", map[string]any{"conversation_id": "conv-2", "generation_id": "turn-2", "prompt": "hello"}},
		{"afterAgentResponse", map[string]any{"conversation_id": "conv-2", "text": "world", "input_tokens": float64(3), "output_tokens": float64(2)}},
		{"stop", map[string]any{"conversation_id": "conv-2", "status": "completed"}},
	} {
		if err := ProcessEvent(item.event, item.payload, options); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/v1/traces"] != 1 || requests["/v1/metrics"] != 1 {
		t.Fatalf("unexpected upload requests: %#v", requests)
	}
	journal, err := journalPath(stateDir, "conv-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("Cursor journal was not cleared: %v", err)
	}
}

func TestPayloadForStorageDropsContentWhenCaptureIsDisabled(t *testing.T) {
	payload := map[string]any{
		"conversation_id": "conv-3",
		"cwd":             "/workspace",
		"prompt":          "do not persist",
		"tool_input":      map[string]any{"X-Token": "agent_secret"},
	}
	stored := payloadForStorage(payload, cursorconfig.Config{CaptureContent: "none", MaxChars: 20_000})
	if stored["conversation_id"] != "conv-3" || stored["cwd"] != "/workspace" {
		t.Fatalf("required event metadata was removed: %#v", stored)
	}
	if stored["prompt"] != nil || stored["tool_input"] != nil {
		t.Fatalf("content was retained with capture disabled: %#v", stored)
	}
}

func TestPayloadForStoragePreservesUsageAndRedactsContentSecrets(t *testing.T) {
	payload := map[string]any{
		"conversation_id": "conv-4",
		"input_tokens":    float64(42),
		"tool_input":      map[string]any{"X-Token": "agent_secret", "command": "echo ok"},
	}
	stored := payloadForStorage(payload, cursorconfig.Config{CaptureContent: "preview", MaxChars: 20_000})
	if stored["input_tokens"] != float64(42) {
		t.Fatalf("token usage was not preserved: %#v", stored)
	}
	toolInput := stored["tool_input"].(map[string]any)
	if toolInput["X-Token"] != "[REDACTED]" || toolInput["command"] != "echo ok" {
		t.Fatalf("tool content was not sanitized correctly: %#v", toolInput)
	}
}
