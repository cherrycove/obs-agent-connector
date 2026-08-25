package parse

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/semantic"
)

func TestReadLatestTurnBuildsKiroToolChain(t *testing.T) {
	sessionDir := t.TempDir()
	sessionID := "session-1"
	end := time.Date(2026, time.August, 21, 10, 0, 5, 0, time.UTC)
	metadata := map[string]any{
		"end_reason":                    "UserTurnEnd",
		"end_timestamp":                 end.Format(time.RFC3339Nano),
		"turn_duration":                 map[string]any{"secs": float64(5), "nanos": float64(0)},
		"input_token_count":             float64(20),
		"output_token_count":            float64(7),
		"cache_read_input_token_count":  float64(3),
		"cache_write_input_token_count": float64(0),
		"message_ids":                   []any{"message-tool", "message-final"},
	}
	sidecar := map[string]any{
		"session_id": sessionID,
		"cwd":        "/workspace",
		"updated_at": end.Format(time.RFC3339Nano),
		"session_state": map[string]any{
			"rts_model_state":       map[string]any{"model_info": map[string]any{"model_id": "claude-sonnet-4"}},
			"conversation_metadata": map[string]any{"user_turn_metadatas": []any{metadata}},
		},
	}
	writeJSON(t, filepath.Join(sessionDir, sessionID+".json"), sidecar)
	lines := []map[string]any{
		{"version": "v1", "kind": "Prompt", "data": map[string]any{
			"content": []any{map[string]any{"kind": "text", "data": "inspect the repository"}},
		}},
		{"version": "v1", "kind": "AssistantMessage", "data": map[string]any{
			"message_id": "message-tool",
			"content": []any{map[string]any{"kind": "toolUse", "data": map[string]any{
				"toolUseId": "tool-1", "name": "shell", "input": map[string]any{"command": "go test ./..."},
			}}},
		}},
		{"version": "v1", "kind": "ToolResults", "data": map[string]any{
			"content": []any{map[string]any{"kind": "toolResult", "data": map[string]any{
				"toolUseId": "tool-1", "content": []any{map[string]any{"kind": "text", "data": "ok"}},
			}}},
		}},
		{"version": "v1", "kind": "AssistantMessage", "data": map[string]any{
			"message_id": "message-final", "content": []any{map[string]any{"kind": "text", "data": "done"}},
		}},
	}
	writeJSONL(t, filepath.Join(sessionDir, sessionID+".jsonl"), lines)
	base := end.Add(-5 * time.Second).UnixNano()
	turn, ok, err := ReadLatestTurn(Options{
		SessionDir: sessionDir, SessionID: sessionID, Cwd: "/workspace", CaptureContent: "preview", MaxChars: 20_000,
		ResourceAttributes: map[string]any{"team": "platform"},
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: base, Payload: map[string]any{"session_id": sessionID, "prompt": "inspect the repository"}},
			{Event: "PreToolUse", RecordedNano: base + int64(time.Second), Payload: map[string]any{"session_id": sessionID, "tool_name": "shell", "tool_input": map[string]any{"command": "go test ./..."}}},
			{Event: "PostToolUse", RecordedNano: base + 2*int64(time.Second), Payload: map[string]any{"session_id": sessionID, "tool_name": "shell", "tool_response": "ok"}},
			{Event: "Stop", RecordedNano: end.UnixNano(), Payload: map[string]any{"session_id": sessionID, "assistant_response": "done"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadLatestTurn() ok=%t err=%v", ok, err)
	}
	if turn.SessionID != sessionID || turn.TurnID != "message-tool" || turn.FinalStatus != model.FinalStatusCompleted {
		t.Fatalf("unexpected turn identity: %#v", turn)
	}
	if turn.InputPreview != "inspect the repository" || turn.OutputPreview != "done" {
		t.Fatalf("unexpected content: %#v", turn)
	}
	if turn.Usage.InputTokens != 20 || turn.Usage.OutputTokens != 7 || turn.Usage.CacheReadTokens != 3 {
		t.Fatalf("unexpected usage: %#v", turn.Usage)
	}
	if len(turn.LLMCalls) != 2 || turn.LLMCalls[0].Usage.InputTokens != 0 {
		t.Fatalf("expected per-call usage to remain unset for aggregate multi-call metadata: %#v", turn.LLMCalls)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].CallID != "tool-1" || turn.ToolCalls[0].Command != "go test ./..." {
		t.Fatalf("unexpected tool calls: %#v", turn.ToolCalls)
	}
	if turn.ToolCalls[0].StartUnixNano != base+int64(time.Second) || turn.ToolCalls[0].EndUnixNano != base+2*int64(time.Second) {
		t.Fatalf("Kiro Hook timing was not used: %#v", turn.ToolCalls[0])
	}
	if turn.Resource["team"] != "platform" || turn.Resource["agent_runtime"] != "kiro" {
		t.Fatalf("unexpected resource attributes: %#v", turn.Resource)
	}
}

func TestReadLatestTurnUsesStopAssistantFallback(t *testing.T) {
	sessionDir := t.TempDir()
	sessionID := "session-2"
	end := time.Date(2026, time.August, 21, 11, 0, 0, 0, time.UTC)
	fallbackMetadata := map[string]any{
		"end_reason": "UserTurnEnd", "end_timestamp": end.Format(time.RFC3339Nano), "turn_duration": map[string]any{"secs": float64(1)},
	}
	writeJSON(t, filepath.Join(sessionDir, sessionID+".json"), map[string]any{
		"session_id": sessionID, "cwd": "/workspace", "updated_at": end.Format(time.RFC3339Nano),
		"session_state": map[string]any{"conversation_metadata": map[string]any{"user_turn_metadatas": []any{fallbackMetadata}}},
	})
	writeJSONL(t, filepath.Join(sessionDir, sessionID+".jsonl"), []map[string]any{
		{"kind": "Prompt", "data": map[string]any{"content": []any{map[string]any{"kind": "text", "data": "hello"}}}},
		{"kind": "AssistantMessage", "data": map[string]any{"message_id": "pending", "content": []any{map[string]any{"kind": "thinking", "data": map[string]any{"text": "internal"}}}}},
	})
	turn, ok, err := ReadLatestTurn(Options{SessionDir: sessionDir, SessionID: sessionID, Cwd: "/workspace", AssistantResponse: "world", CaptureContent: "preview", MaxChars: 20_000})
	if err != nil || !ok {
		t.Fatalf("ReadLatestTurn() ok=%t err=%v", ok, err)
	}
	if turn.OutputPreview != "world" || len(turn.AssistantOutputs) != 1 {
		t.Fatalf("Stop fallback was not used: %#v", turn)
	}
}

func TestReadLatestTurnSkipsNonTerminalBlankSession(t *testing.T) {
	sessionDir := t.TempDir()
	sessionID := "session-3"
	writeJSON(t, filepath.Join(sessionDir, sessionID+".json"), map[string]any{"session_id": sessionID, "cwd": "/workspace"})
	writeJSONL(t, filepath.Join(sessionDir, sessionID+".jsonl"), []map[string]any{{"kind": "Prompt", "data": map[string]any{"content": []any{map[string]any{"kind": "text", "data": "hello"}}}}})
	_, ok, err := ReadLatestTurn(Options{SessionDir: sessionDir, SessionID: sessionID, Cwd: "/workspace", CaptureContent: "preview", MaxChars: 20_000})
	if err != nil || ok {
		t.Fatalf("expected blank session to be skipped, ok=%t err=%v", ok, err)
	}
}

func TestReadLatestTurnBuildsModernKiroTurn(t *testing.T) {
	sessionRoot := t.TempDir()
	sessionID := "sess_modern"
	executionID := "execution-current"
	start := time.Date(2026, time.August, 24, 9, 30, 0, 0, time.UTC)
	end := start.Add(6 * time.Second)
	writeModernSession(t, sessionRoot, "workspace-hash", sessionID, "auto", []map[string]any{
		modernRecord("user-1", start, map[string]any{"type": "user", "content": "inspect synthetic fixtures"}),
		modernRecord("turn-start", start.Add(time.Second), map[string]any{"type": "turn_start", "executionId": executionID}),
		modernRecord("reasoning", start.Add(2*time.Second), map[string]any{"type": "assistant", "executionId": executionID, "operationType": "Reasoning", "content": "private reasoning"}),
		modernRecord("subagent-say", start.Add(2200*time.Millisecond), map[string]any{"type": "assistant", "executionId": executionID, "subExecutionId": "subagent-1", "operationType": "Say", "content": "subagent internal output"}),
		modernRecord("say-1", start.Add(2500*time.Millisecond), map[string]any{"type": "assistant", "executionId": executionID, "operationType": "Say", "content": "checking"}),
		modernRecord("tool-1-start", start.Add(3*time.Second), map[string]any{"type": "tool_call", "executionId": executionID, "toolCallId": "tool-1", "toolName": "execute_bash", "args": map[string]any{"command": "go test ./..."}, "status": "executing"}),
		modernRecord("tool-1-result", start.Add(4*time.Second), map[string]any{"type": "tool_result", "executionId": executionID, "toolCallId": "tool-1", "content": "ok", "success": true, "durationMs": float64(500)}),
		modernRecord("say-2", start.Add(5*time.Second), map[string]any{"type": "assistant", "executionId": executionID, "operationType": "Say", "content": "done"}),
		modernRecord("usage", start.Add(5500*time.Millisecond), map[string]any{
			"type": "usage_summary", "executionId": executionID, "status": "success",
			"requestIds": []any{"request-1", "request-2"},
			"promptTurnSummaries": []any{
				map[string]any{"usage": 0.25, "unit": "credit"},
				map[string]any{"usage": 0.20, "unitPlural": "credits"},
				map[string]any{"usage": 99.0, "unit": "request"},
			},
		}),
		modernRecord("turn-end", end, map[string]any{"type": "turn_end", "executionId": executionID, "stopReason": "end_turn"}),
	})

	preTime := start.Add(3100 * time.Millisecond).UnixNano()
	postTime := start.Add(4200 * time.Millisecond).UnixNano()
	turn, ok, err := ReadLatestTurn(Options{
		SessionDir: sessionRoot, SessionID: sessionID, Cwd: "/workspace", CaptureContent: "preview", MaxChars: 20_000,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start.UnixNano(), Payload: map[string]any{"session_id": sessionID, "prompt": "inspect synthetic fixtures"}},
			{Event: "PreToolUse", RecordedNano: preTime, Payload: map[string]any{"tool_use_id": "tool-1", "tool_name": "execute_bash"}},
			{Event: "PostToolUse", RecordedNano: postTime, Payload: map[string]any{"tool_use_id": "tool-1", "tool_name": "execute_bash"}},
			{Event: "Stop", RecordedNano: end.Add(time.Second).UnixNano(), Payload: map[string]any{"session_id": sessionID}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadLatestTurn() ok=%t err=%v", ok, err)
	}
	if turn.SessionID != sessionID || turn.TurnID != executionID || turn.StartUnixNano != start.UnixNano() || turn.EndUnixNano != end.UnixNano() {
		t.Fatalf("unexpected modern turn identity or timing: %#v", turn)
	}
	if turn.OutputPreview != "checking done" || len(turn.LLMCalls) != 2 || turn.LLMCalls[0].CallID != "request-1" || turn.LLMCalls[1].CallID != "request-2" {
		t.Fatalf("unexpected modern assistant or LLM calls: %#v", turn)
	}
	if turn.Usage != (model.Usage{}) || turn.LLMCalls[0].Usage != (model.Usage{}) {
		t.Fatalf("Kiro credits must not be exported as token usage: root=%#v calls=%#v", turn.Usage, turn.LLMCalls)
	}
	credit, ok := turn.ExtraAttributes["gen_ai.usage.credit"].(float64)
	if !ok || math.Abs(credit-0.45) > 1e-9 {
		t.Fatalf("unexpected Kiro credit usage: %#v", turn.ExtraAttributes)
	}
	spans := (semantic.Builder{ScopeVersion: "test"}).Build(turn)
	if len(spans) == 0 || spans[0].Name != "invoke_agent" {
		t.Fatalf("Kiro credit usage was not exported on invoke_agent: %#v", spans)
	}
	rootCredit, ok := spans[0].Attributes["gen_ai.usage.credit"].(float64)
	if !ok || math.Abs(rootCredit-0.45) > 1e-9 {
		t.Fatalf("unexpected invoke_agent credit usage: %#v", spans[0].Attributes)
	}
	for _, span := range spans[1:] {
		if _, exists := span.Attributes["gen_ai.usage.credit"]; exists {
			t.Fatalf("Kiro credit usage must only be exported on invoke_agent: %#v", span)
		}
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].CallID != "tool-1" || turn.ToolCalls[0].TriggeringLLMCall != "request-1" {
		t.Fatalf("unexpected modern tool call: %#v", turn.ToolCalls)
	}
	if turn.ToolCalls[0].StartUnixNano != preTime || turn.ToolCalls[0].EndUnixNano != postTime || turn.ToolCalls[0].Command != "go test ./..." {
		t.Fatalf("Hook evidence did not override modern tool timing: %#v", turn.ToolCalls[0])
	}
}

func TestModernCreditUsageRequiresExplicitCreditUnit(t *testing.T) {
	usage := modernCreditUsage([]any{
		map[string]any{"usage": 3.0},
		map[string]any{"usage": 4.0, "unit": "request"},
		map[string]any{"usage": -1.0, "unit": "credit"},
	})
	if usage != 0 {
		t.Fatalf("non-credit summaries must be ignored, got %v", usage)
	}
}

func TestReadLatestTurnAcceptsModernCancelledTurnWithoutAssistant(t *testing.T) {
	sessionRoot := t.TempDir()
	sessionID := "sess_cancelled"
	start := time.Date(2026, time.August, 24, 10, 0, 0, 0, time.UTC)
	writeModernSession(t, sessionRoot, "workspace-hash", sessionID, "auto", []map[string]any{
		modernRecord("user", start, map[string]any{"type": "user", "content": "cancel this synthetic task"}),
		modernRecord("start", start.Add(time.Second), map[string]any{"type": "turn_start", "executionId": "execution-cancelled"}),
		modernRecord("usage", start.Add(2*time.Second), map[string]any{"type": "usage_summary", "executionId": "execution-cancelled", "status": "aborted", "requestIds": []any{"request-cancelled"}}),
		modernRecord("end", start.Add(3*time.Second), map[string]any{"type": "turn_end", "executionId": "execution-cancelled", "stopReason": "cancelled"}),
	})
	turn, ok, err := ReadLatestTurn(Options{SessionDir: sessionRoot, SessionID: sessionID, CaptureContent: "preview", MaxChars: 20_000})
	if err != nil || !ok {
		t.Fatalf("ReadLatestTurn() ok=%t err=%v", ok, err)
	}
	if turn.FinalStatus != model.FinalStatusCancelled || len(turn.LLMCalls) != 1 || len(turn.AssistantOutputs) != 0 {
		t.Fatalf("unexpected cancelled turn: %#v", turn)
	}
}

func TestReadLatestTurnNeverFallsBackToDifferentLegacySession(t *testing.T) {
	sessionRoot := t.TempDir()
	legacyDir := filepath.Join(sessionRoot, "cli")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(legacyDir, "stale-session.json"), map[string]any{
		"session_id": "stale-session", "cwd": "/same-workspace", "updated_at": time.Now().Format(time.RFC3339Nano),
	})
	writeJSONL(t, filepath.Join(legacyDir, "stale-session.jsonl"), []map[string]any{
		{"kind": "Prompt", "data": map[string]any{"content": []any{map[string]any{"kind": "text", "data": "stale prompt"}}}},
		{"kind": "AssistantMessage", "data": map[string]any{"message_id": "stale-turn", "content": []any{map[string]any{"kind": "text", "data": "stale output"}}}},
	})
	_, ok, err := ReadLatestTurn(Options{SessionDir: sessionRoot, SessionID: "sess_current", Cwd: "/same-workspace", CaptureContent: "preview", MaxChars: 20_000})
	if ok || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected exact-session miss, ok=%t err=%v", ok, err)
	}
}

func TestReadLatestTurnIgnoresIncompleteModernJSONLTail(t *testing.T) {
	sessionRoot := t.TempDir()
	sessionID := "sess_partial_tail"
	start := time.Date(2026, time.August, 24, 10, 30, 0, 0, time.UTC)
	messagesPath := writeModernSession(t, sessionRoot, "workspace-hash", sessionID, "auto", []map[string]any{
		modernRecord("user", start, map[string]any{"type": "user", "content": "complete synthetic turn"}),
		modernRecord("start", start.Add(time.Second), map[string]any{"type": "turn_start", "executionId": "execution-complete"}),
		modernRecord("say", start.Add(2*time.Second), map[string]any{"type": "assistant", "executionId": "execution-complete", "operationType": "Say", "content": "complete"}),
		modernRecord("end", start.Add(3*time.Second), map[string]any{"type": "turn_end", "executionId": "execution-complete", "stopReason": "end_turn"}),
	})
	file, err := os.OpenFile(messagesPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"id":"partial","payload":`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	turn, ok, err := ReadLatestTurn(Options{SessionDir: sessionRoot, SessionID: sessionID, CaptureContent: "preview", MaxChars: 20_000})
	if err != nil || !ok || turn.TurnID != "execution-complete" {
		t.Fatalf("complete turn was lost after partial tail: turn=%#v ok=%t err=%v", turn, ok, err)
	}
}

func TestReadLatestTurnUsesHookTimingWhenStoredPromptWasSanitized(t *testing.T) {
	sessionRoot := t.TempDir()
	sessionID := "sess_prompt_sanitized"
	firstStart := time.Date(2026, time.August, 24, 11, 0, 0, 0, time.UTC)
	secondStart := firstStart.Add(10 * time.Minute)
	writeModernSession(t, sessionRoot, "workspace-hash", sessionID, "auto", []map[string]any{
		modernRecord("user-1", firstStart, map[string]any{"type": "user", "content": "original sensitive prompt"}),
		modernRecord("start-1", firstStart.Add(time.Second), map[string]any{"type": "turn_start", "executionId": "execution-first"}),
		modernRecord("say-1", firstStart.Add(2*time.Second), map[string]any{"type": "assistant", "executionId": "execution-first", "operationType": "Say", "content": "first output"}),
		modernRecord("end-1", firstStart.Add(3*time.Second), map[string]any{"type": "turn_end", "executionId": "execution-first", "stopReason": "end_turn"}),
		modernRecord("user-2", secondStart, map[string]any{"type": "user", "content": "newer prompt"}),
		modernRecord("start-2", secondStart.Add(time.Second), map[string]any{"type": "turn_start", "executionId": "execution-second"}),
		modernRecord("say-2", secondStart.Add(2*time.Second), map[string]any{"type": "assistant", "executionId": "execution-second", "operationType": "Say", "content": "second output"}),
		modernRecord("end-2", secondStart.Add(3*time.Second), map[string]any{"type": "turn_end", "executionId": "execution-second", "stopReason": "end_turn"}),
	})
	turn, ok, err := ReadLatestTurn(Options{
		SessionDir: sessionRoot, SessionID: sessionID, CaptureContent: "preview", MaxChars: 20_000,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: firstStart.Add(500 * time.Millisecond).UnixNano(), Payload: map[string]any{"prompt": "[REDACTED]"}},
			{Event: "Stop", RecordedNano: firstStart.Add(4 * time.Second).UnixNano(), Payload: map[string]any{}},
		},
	})
	if err != nil || !ok || turn.TurnID != "execution-first" {
		t.Fatalf("Hook timing did not select the intended terminal turn: turn=%#v ok=%t err=%v", turn, ok, err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSONL(t *testing.T, path string, values []map[string]any) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
	}
}

func writeModernSession(t *testing.T, root, bucket, sessionID, modelID string, records []map[string]any) string {
	t.Helper()
	directory := filepath.Join(root, bucket, sessionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(directory, "session.json"), map[string]any{
		"id": sessionID, "modelId": modelID, "status": "idle", "workspacePaths": []any{"/workspace"},
	})
	messagesPath := filepath.Join(directory, "messages.jsonl")
	writeJSONL(t, messagesPath, records)
	return messagesPath
}

func modernRecord(id string, timestamp time.Time, payload map[string]any) map[string]any {
	return map[string]any{"id": id, "timestamp": timestamp.Format(time.RFC3339Nano), "payload": payload}
}
