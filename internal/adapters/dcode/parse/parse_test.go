package parse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
)

func TestReadTurnBuildsDcodeToolChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-1.jsonl")
	writeTranscript(t, path, []map[string]any{
		{"schema_version": 1, "sequence": 0, "record_id": "user-1", "thread_id": "thread-1", "role": "user", "message_id": "user-1", "content": "inspect the repository"},
		{"schema_version": 1, "sequence": 1, "record_id": "assistant-tool", "thread_id": "thread-1", "role": "assistant", "message_id": "assistant-tool", "content": []any{}},
		{"schema_version": 1, "sequence": 2, "record_id": "tool-result", "thread_id": "thread-1", "role": "tool", "message_id": "tool-result", "content": "ok", "name": "execute"},
		{"schema_version": 1, "sequence": 3, "record_id": "assistant-final", "thread_id": "thread-1", "role": "assistant", "message_id": "assistant-final", "content": "done"},
	})
	start := time.Date(2026, time.August, 24, 10, 0, 0, 0, time.UTC).UnixNano()
	end := start + 5*int64(time.Second)
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "thread-1", TurnID: "prompt-1", Cwd: "/workspace",
		LastAssistant: "done", CaptureContent: "preview", MaxChars: 20_000,
		ResourceAttributes: map[string]any{"team": "platform"},
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"session_id": "thread-1", "prompt_id": "prompt-1", "prompt": "inspect the repository"}},
			{Event: "PreToolUse", RecordedNano: start + int64(time.Second), Payload: map[string]any{"tool_use_id": "tool-1", "tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./..."}}},
			{Event: "PostToolUse", RecordedNano: start + 3*int64(time.Second), Payload: map[string]any{"tool_use_id": "tool-1", "tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./..."}, "tool_response": "ok", "duration_ms": float64(1250)}},
			{Event: "Stop", RecordedNano: end, Payload: map[string]any{"session_id": "thread-1", "prompt_id": "prompt-1", "last_assistant_message": "done"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if turn.SessionID != "thread-1" || turn.TurnID != "prompt-1" || turn.FinalStatus != model.FinalStatusCompleted {
		t.Fatalf("unexpected turn identity: %#v", turn)
	}
	if turn.InputPreview != "inspect the repository" || turn.OutputPreview != "done" {
		t.Fatalf("unexpected content: %#v", turn)
	}
	if len(turn.LLMCalls) != 2 || turn.LLMCalls[0].CallID != "assistant-tool" || turn.LLMCalls[1].CallID != "assistant-final" {
		t.Fatalf("unexpected LLM calls: %#v", turn.LLMCalls)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].CallID != "tool-1" || turn.ToolCalls[0].Command != "go test ./..." {
		t.Fatalf("unexpected tool calls: %#v", turn.ToolCalls)
	}
	if turn.ToolCalls[0].TriggeringLLMCall != "assistant-tool" {
		t.Fatalf("tool was not correlated with its assistant message: %#v", turn.ToolCalls[0])
	}
	if got := turn.ToolCalls[0].EndUnixNano - turn.ToolCalls[0].StartUnixNano; got != 1250*int64(time.Millisecond) {
		t.Fatalf("dcode duration_ms was not used: %d", got)
	}
	if turn.Resource["team"] != "platform" || turn.Resource["agent_runtime"] != "dcode" {
		t.Fatalf("unexpected resource attributes: %#v", turn.Resource)
	}
	if turn.Usage.InputTokens != 0 || turn.LLMCalls[0].Usage.InputTokens != 0 {
		t.Fatalf("dcode must not fabricate token usage: %#v", turn)
	}
}

func TestReadTurnUsesStopOutputAndSubagentIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-2.jsonl")
	writeTranscript(t, path, []map[string]any{
		{"schema_version": 1, "sequence": 0, "record_id": "user-2", "thread_id": "thread-2", "role": "user", "content": "delegate this"},
		{"schema_version": 1, "sequence": 1, "record_id": "assistant-tool", "thread_id": "thread-2", "role": "assistant", "content": []any{}},
	})
	start := time.Now().Add(-time.Second).UnixNano()
	end := time.Now().UnixNano()
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "thread-2", TurnID: "prompt-2", LastAssistant: "completed by a subagent",
		CaptureContent: "preview", MaxChars: 20_000,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "delegate this"}},
			{Event: "PreToolUse", RecordedNano: start + 1, Payload: map[string]any{"tool_use_id": "task-1", "tool_name": "Task", "tool_input": map[string]any{"subagent_type": "researcher"}}},
			{Event: "SubagentStart", RecordedNano: start + 2, Payload: map[string]any{"agent_id": "task-1", "agent_type": "researcher"}},
			{Event: "PostToolUse", RecordedNano: end - 1, Payload: map[string]any{"tool_use_id": "task-1", "tool_name": "Task", "tool_response": "result"}},
			{Event: "SubagentStop", RecordedNano: end - 1, Payload: map[string]any{"agent_id": "task-1", "agent_type": "researcher"}},
			{Event: "Stop", RecordedNano: end, Payload: map[string]any{"last_assistant_message": "completed by a subagent"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if turn.OutputPreview != "completed by a subagent" || len(turn.AssistantOutputs) != 1 {
		t.Fatalf("Stop output was not used: %#v", turn)
	}
	if got := turn.ToolCalls[0].ExtraAttributes["gen_ai.subagent.name"]; got != "researcher" {
		t.Fatalf("subagent identity was not correlated: %#v", turn.ToolCalls[0])
	}
}

func TestReadTurnMarksToolFailureAndHonorsCaptureNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-3.jsonl")
	writeTranscript(t, path, []map[string]any{
		{"schema_version": 1, "sequence": 0, "record_id": "user-3", "thread_id": "thread-3", "role": "user", "content": "run it"},
		{"schema_version": 1, "sequence": 1, "record_id": "assistant-3", "thread_id": "thread-3", "role": "assistant", "content": "could not run"},
	})
	start := time.Now().Add(-time.Second).UnixNano()
	end := time.Now().UnixNano()
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "thread-3", TurnID: "prompt-3", LastAssistant: "could not run",
		CaptureContent: "none", MaxChars: 20_000,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "run it"}},
			{Event: "PreToolUse", RecordedNano: start + 1, Payload: map[string]any{"tool_use_id": "tool-fail", "tool_name": "Bash"}},
			{Event: "PostToolUseFailure", RecordedNano: end - 1, Payload: map[string]any{"tool_use_id": "tool-fail", "tool_name": "Bash", "error": "interrupted by user", "is_interrupt": true}},
			{Event: "Stop", RecordedNano: end, Payload: map[string]any{}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if turn.InputMessages != nil || turn.OutputMessages != nil || turn.ToolCalls[0].Arguments != nil {
		t.Fatalf("capture=none retained content: %#v", turn)
	}
	if turn.ToolCalls[0].Status != "error" || turn.ToolCalls[0].ErrorType != "cancelled" || turn.ToolCalls[0].ResultStatus != "cancelled" {
		t.Fatalf("tool failure was not represented: %#v", turn.ToolCalls[0])
	}
	if turn.FinalStatus != model.FinalStatusCancelled || turn.ToolCalls[0].ExtraAttributes["is_interrupt"] != true {
		t.Fatalf("tool interruption did not cancel the turn: %#v", turn)
	}
}

func TestReadTurnSkipsUserOnlyTurnWithoutTerminalEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-4.jsonl")
	writeTranscript(t, path, []map[string]any{
		{"schema_version": 1, "sequence": 0, "record_id": "user-4", "thread_id": "thread-4", "role": "user", "content": "hello"},
	})
	_, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "thread-4", TurnID: "prompt-4", CaptureContent: "preview", MaxChars: 20_000,
		Events: []JournalEvent{{Event: "UserPromptSubmit", RecordedNano: time.Now().UnixNano(), Payload: map[string]any{"prompt": "hello"}}},
	})
	if err != nil || ok {
		t.Fatalf("expected blank turn to be skipped, ok=%t err=%v", ok, err)
	}
}

func TestReadTurnBuildsErrorFromFailedSessionEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-5.jsonl")
	writeTranscript(t, path, []map[string]any{
		{"schema_version": 1, "sequence": 0, "record_id": "user-5", "thread_id": "thread-5", "role": "user", "content": "hello"},
	})
	start := time.Now().Add(-5 * time.Second).UnixNano()
	end := time.Now().UnixNano()
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "thread-5", TurnID: "prompt-5", CaptureContent: "none", MaxChars: 20_000,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "hello"}},
			{Event: "SessionEnd", RecordedNano: end, Payload: map[string]any{"reason": "other"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if turn.ErrorType != "dcode_agent_error" || turn.FinalStatus != model.FinalStatusCompleted {
		t.Fatalf("failed SessionEnd was not represented as a terminal error: %#v", turn)
	}
	if turn.StartUnixNano != start || turn.EndUnixNano != end || turn.ExtraAttributes["dcode.session_end.reason"] != "other" {
		t.Fatalf("unexpected failed SessionEnd timing or evidence: %#v", turn)
	}
	if turn.InputMessages != nil || turn.InputPreview != "" || len(turn.LLMCalls) != 0 || len(turn.AssistantOutputs) != 0 {
		t.Fatalf("failed SessionEnd fabricated content or child spans: %#v", turn)
	}
}

func writeTranscript(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
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
