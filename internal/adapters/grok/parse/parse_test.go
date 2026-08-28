package parse

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coremetrics "github.com/GuanceCloud/obs-agent-connector/internal/core/metrics"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/semantic"
)

func TestSyntheticFixturesRemainSanitizedAndUsable(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join("testdata", entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, forbidden := range []string{"/Users/", `C:\\Users\\`, "Authorization", "Bearer ", "api_key", "private key"} {
			if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
				t.Fatalf("fixture %s contains forbidden real-world secret/path marker %q", path, forbidden)
			}
		}
		if entry.Name() == "updates_incomplete_tail.jsonl" {
			continue
		}
		scanner := bufio.NewScanner(strings.NewReader(text))
		for scanner.Scan() {
			if !json.Valid(scanner.Bytes()) {
				t.Fatalf("fixture %s contains invalid JSON: %s", path, scanner.Text())
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := CompletedTurnIDs(filepath.Join("testdata", "updates_incomplete_tail.jsonl"), "synthetic-tail-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "synthetic-tail-prompt" {
		t.Fatalf("incomplete-tail fixture hid its durable turn: %#v", ids)
	}
}

func TestReadTurnRequiresDurableTerminalAndBuildsPairedResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		xaiUpdate(100, "session-1", map[string]any{"sessionUpdate": "response_started", "message_id": "message-1", "model": "grok-code", "input_tokens": 10, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}),
		xaiUpdate(101, "session-1", map[string]any{"sessionUpdate": "response_completed", "message_id": "message-1", "stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2, "reasoning_tokens": 1}}),
		xaiUpdate(102, "session-1", map[string]any{"sessionUpdate": "response_started", "message_id": "message-2", "model": "grok-code", "input_tokens": 20}),
		xaiUpdate(103, "session-1", map[string]any{"sessionUpdate": "response_completed", "message_id": "message-2", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 20, "output_tokens": 8}}),
		xaiUpdate(104, "session-1", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-1", "stop_reason": "end_turn", "agent_result": "done", "usage": map[string]any{"inputTokens": 35, "outputTokens": 12, "cachedReadTokens": 3, "cacheCreationTokens": 2, "modelCalls": 2}}),
	})
	start := time.Unix(99, 0).UnixNano()
	end := time.Unix(104, 1).UnixNano()
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-1", TurnID: "prompt-1", AgentVersion: "1.0.5",
		CaptureContent: "preview", MaxChars: 20_000, ResourceAttributes: map[string]any{"team": "platform"},
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "inspect the skill"}},
			{Event: "PreToolUse", RecordedNano: start + int64(time.Second), Payload: map[string]any{"toolUseId": "tool-1", "toolName": "read_file", "toolInput": map[string]any{"file_path": "/synthetic/skills/example/SKILL.md"}}},
			{Event: "PostToolUse", RecordedNano: start + 2*int64(time.Second), Payload: map[string]any{"toolUseId": "tool-1", "toolName": "read_file", "toolResult": "contents", "durationMs": 250}},
			{Event: "Stop", RecordedNano: end, Payload: map[string]any{"reason": "end_turn", "stopHookActive": false, "lastAssistantMessage": "done"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if turn.SessionID != "session-1" || turn.TurnID != "prompt-1" || turn.FinalStatus != model.FinalStatusCompleted {
		t.Fatalf("unexpected identity or status: %#v", turn)
	}
	if turn.AgentRuntime != "grok" || turn.AgentName != "Grok Build" || turn.AgentVersion != "1.0.5" {
		t.Fatalf("unexpected Grok identity: %#v", turn)
	}
	if len(turn.LLMCalls) != 2 || turn.LLMCalls[0].CallID != "message-1" || turn.LLMCalls[1].CallID != "message-2" {
		t.Fatalf("response boundaries were not preserved: %#v", turn.LLMCalls)
	}
	if got := turn.LLMCalls[0].Usage; got.InputTokens != 15 || got.OutputTokens != 4 || got.CacheReadTokens != 3 || got.CacheCreateTokens != 2 || got.ReasoningTokens != 1 {
		t.Fatalf("per-call response usage was not mapped correctly: %#v", got)
	}
	if turn.Usage.InputTokens != 35 || turn.Usage.OutputTokens != 12 {
		t.Fatalf("aggregate usage was not kept at turn level: %#v", turn.Usage)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Skill == nil || turn.ToolCalls[0].Skill.Name != "example" {
		t.Fatalf("reliable SKILL.md read was not mapped: %#v", turn.ToolCalls)
	}
	if turn.ToolCalls[0].Skill.Path != "/synthetic/skills/example/SKILL.md" || turn.ToolCalls[0].Command != "" {
		t.Fatalf("unexpected skill/tool content: %#v", turn.ToolCalls[0])
	}
	if len(turn.AssistantOutputs) != 1 || turn.OutputPreview != "done" || turn.Resource["team"] != "platform" {
		t.Fatalf("unexpected output or resource: %#v", turn)
	}
	spans := (semantic.Builder{ScopeName: "gtrace-grok-test", ScopeVersion: "test"}).Build(turn)
	if len(spans) != 6 || spans[0].Name != "invoke_agent" {
		t.Fatalf("unexpected Grok trace shape: %#v", spans)
	}
	rootID := spans[0].SpanID
	toolID := ""
	for _, span := range spans[1:] {
		switch span.Name {
		case "llm", "tool:read_file", "assistant":
			if span.ParentID != rootID {
				t.Fatalf("%s must be a direct root child: %#v", span.Name, span)
			}
		case "skill:example":
			if toolID == "" || span.ParentID != toolID {
				t.Fatalf("skill must be nested under its tool: %#v", span)
			}
		}
		if span.Name == "tool:read_file" {
			toolID = span.SpanID
		}
		if span.Name == "assistant" && span.Attributes["gen_ai.usage.input_tokens"] != nil {
			t.Fatalf("assistant span carried token usage: %#v", span.Attributes)
		}
	}
	allowedMetrics := map[string]bool{
		"gen_ai.workflow.duration": true, "gen_ai.agent.operation.count": true,
		"gen_ai.agent.operation.duration": true, "gen_ai.client.token.usage": true,
	}
	tokenPoints := 0
	for _, metric := range coremetrics.Build(spans) {
		if !allowedMetrics[metric.Name] {
			t.Fatalf("unexpected metric %q", metric.Name)
		}
		if metric.Name == "gen_ai.client.token.usage" {
			tokenPoints++
		}
	}
	if tokenPoints != 4 {
		t.Fatalf("expected input/output token points for exactly two LLM calls, got %d", tokenPoints)
	}
}

func TestReadTurnDoesNotInventLLMFromUnpairedCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		xaiUpdate(200, "session-2", map[string]any{"sessionUpdate": "response_completed", "message_id": "message-only", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 100, "output_tokens": 50}}),
		xaiUpdate(201, "session-2", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-2", "stop_reason": "end_turn", "agent_result": "answer", "usage": map[string]any{"inputTokens": 100, "outputTokens": 50}}),
	})
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-2", TurnID: "prompt-2", CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{{Event: "UserPromptSubmit", RecordedNano: time.Unix(199, 0).UnixNano(), Payload: map[string]any{"prompt": "hello"}}},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.LLMCalls) != 0 {
		t.Fatalf("unpaired ResponseCompleted fabricated an LLM call: %#v", turn.LLMCalls)
	}
	if turn.Usage.InputTokens != 100 || turn.OutputPreview != "answer" {
		t.Fatalf("durable turn data was lost: %#v", turn)
	}
}

func TestReadTurnPairsOnlyUnambiguousResponsesWithoutMessageIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		xaiUpdate(210, "session-no-id", map[string]any{"sessionUpdate": "response_started", "model": "grok-code", "input_tokens": 3}),
		xaiUpdate(211, "session-no-id", map[string]any{"sessionUpdate": "response_completed", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}}),
		xaiUpdate(212, "session-no-id", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-no-id", "stop_reason": "end_turn", "agent_result": "answer"}),
	})
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-no-id", TurnID: "prompt-no-id", CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{{Event: "UserPromptSubmit", RecordedNano: time.Unix(209, 0).UnixNano(), Payload: map[string]any{"prompt": "hello"}}},
	})
	if err != nil || !ok || len(turn.LLMCalls) != 1 {
		t.Fatalf("strict no-ID pair was not accepted: ok=%t err=%v calls=%#v", ok, err, turn.LLMCalls)
	}
	if turn.LLMCalls[0].Usage.InputTokens != 3 || turn.LLMCalls[0].Usage.OutputTokens != 2 {
		t.Fatalf("strict no-ID usage was not preserved: %#v", turn.LLMCalls[0])
	}

	ambiguousPath := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, ambiguousPath, []map[string]any{
		xaiUpdate(220, "session-ambiguous", map[string]any{"sessionUpdate": "response_started", "input_tokens": 3}),
		xaiUpdate(221, "session-ambiguous", map[string]any{"sessionUpdate": "response_started", "input_tokens": 4}),
		xaiUpdate(222, "session-ambiguous", map[string]any{"sessionUpdate": "response_completed", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 4, "output_tokens": 2}}),
		xaiUpdate(223, "session-ambiguous", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-ambiguous", "stop_reason": "end_turn", "agent_result": "answer"}),
	})
	ambiguous, ok, err := ReadTurn(Options{
		TranscriptPath: ambiguousPath, SessionID: "session-ambiguous", TurnID: "prompt-ambiguous", CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{{Event: "UserPromptSubmit", RecordedNano: time.Unix(219, 0).UnixNano(), Payload: map[string]any{"prompt": "hello"}}},
	})
	if err != nil || !ok {
		t.Fatalf("ambiguous turn did not normalize: ok=%t err=%v", ok, err)
	}
	if len(ambiguous.LLMCalls) != 0 {
		t.Fatalf("ambiguous no-ID responses fabricated a call: %#v", ambiguous.LLMCalls)
	}
}

func TestReadTurnNormalStopWaitsForMatchingTurnCompleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		xaiUpdate(300, "session-3", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "another-prompt", "stop_reason": "end_turn"}),
	})
	_, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-3", TurnID: "prompt-3", CaptureContent: "none", MaxChars: 100,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: time.Now().Add(-time.Second).UnixNano(), Payload: map[string]any{}},
			{Event: "Stop", RecordedNano: time.Now().UnixNano(), Payload: map[string]any{"stopHookActive": false}},
		},
	})
	if err != nil || ok {
		t.Fatalf("normal Stop without matching durable terminal must remain pending: ok=%t err=%v", ok, err)
	}
}

func TestReadTurnKeepsContentFreeTerminalWithoutResponseBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		xaiUpdate(310, "session-content-free", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-content-free", "stop_reason": "end_turn", "agent_result": "synthetic answer"}),
	})
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-content-free", TurnID: "prompt-content-free", CaptureContent: "none", MaxChars: 100,
		Events: []JournalEvent{{Event: "UserPromptSubmit", RecordedNano: time.Unix(309, 0).UnixNano(), Payload: map[string]any{}}},
	})
	if err != nil || !ok {
		t.Fatalf("content-free durable turn was dropped: ok=%t err=%v", ok, err)
	}
	if turn.InputMessages != nil || turn.OutputMessages != nil || turn.InputPreview != "" || turn.OutputPreview != "" {
		t.Fatalf("capture=none retained content: %#v", turn)
	}
	if turn.OutputLength != len([]rune("synthetic answer")) || len(turn.AssistantOutputs) != 1 || len(turn.LLMCalls) != 0 {
		t.Fatalf("content-free evidence was not preserved without fabricating LLM calls: %#v", turn)
	}
}

func TestReadTurnSupportsExplicitFailureAndCancellationWithoutTranscript(t *testing.T) {
	start := time.Now().Add(-time.Second).UnixNano()
	end := time.Now().UnixNano()
	failure, ok, err := ReadTurn(Options{
		SessionID: "session-failure", TurnID: "prompt-failure", CaptureContent: "none", MaxChars: 100,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{}},
			{Event: "StopFailure", RecordedNano: end, Payload: map[string]any{"error": "rate_limit", "errorDetails": "retry later"}},
		},
	})
	if err != nil || !ok || failure.FinalStatus != model.FinalStatusCompleted || failure.ErrorType != "grok_rate_limit" {
		t.Fatalf("explicit failure was not normalized: ok=%t err=%v turn=%#v", ok, err, failure)
	}
	if failure.InputMessages != nil || len(failure.LLMCalls) != 0 || len(failure.AssistantOutputs) != 0 {
		t.Fatalf("failure fabricated content or LLM boundaries: %#v", failure)
	}

	cancelled, ok, err := ReadTurn(Options{
		SessionID: "session-cancel", TurnID: "prompt-cancel", CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "stop now"}},
			{Event: "StopCancelled", RecordedNano: end, Payload: map[string]any{"reason": "user_interrupt", "cancelledBy": "user", "lastAssistantMessage": "partial"}},
		},
	})
	if err != nil || !ok || cancelled.FinalStatus != model.FinalStatusCancelled || cancelled.ErrorType != "cancelled" {
		t.Fatalf("explicit cancellation was not normalized: ok=%t err=%v turn=%#v", ok, err, cancelled)
	}
	if cancelled.ExtraAttributes["grok.cancelled_by"] != "user" || cancelled.OutputPreview != "partial" {
		t.Fatalf("cancellation evidence was not preserved: %#v", cancelled)
	}
}

func TestReadTurnExplicitTerminalOutranksLaterNormalStop(t *testing.T) {
	start := time.Now().Add(-time.Second).UnixNano()
	end := time.Now().UnixNano()
	for _, test := range []struct {
		name       string
		explicit   JournalEvent
		wantStatus model.FinalStatus
		wantError  string
	}{
		{
			name:       "failure",
			explicit:   JournalEvent{Event: "StopFailure", RecordedNano: end - 1, Payload: map[string]any{"error": "rate_limit"}},
			wantStatus: model.FinalStatusCompleted,
			wantError:  "grok_rate_limit",
		},
		{
			name:       "cancellation",
			explicit:   JournalEvent{Event: "StopCancelled", RecordedNano: end - 1, Payload: map[string]any{"reason": "user_interrupt"}},
			wantStatus: model.FinalStatusCancelled,
			wantError:  "cancelled",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			turn, ok, err := ReadTurn(Options{
				SessionID: "session-terminal", TurnID: "prompt-terminal", CaptureContent: "none", MaxChars: 100,
				Events: []JournalEvent{
					{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{}},
					test.explicit,
					{Event: "Stop", RecordedNano: end, Payload: map[string]any{"reason": "end_turn", "stopHookActive": false}},
				},
			})
			if err != nil || !ok {
				t.Fatalf("explicit terminal was hidden by later Stop: ok=%t err=%v", ok, err)
			}
			if turn.FinalStatus != test.wantStatus || turn.ErrorType != test.wantError {
				t.Fatalf("unexpected explicit terminal result: %#v", turn)
			}
		})
	}
}

func TestCompletedTurnIDsIgnoresUnknownAndIncompleteTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		xaiUpdate(400, "session-4", map[string]any{"sessionUpdate": "unknown_future", "value": true}),
		xaiUpdate(401, "other-session", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "other"}),
		xaiUpdate(402, "session-4", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-4"}),
		xaiUpdate(403, "session-4", map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "prompt-4"}),
	})
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"timestamp":404,"method":"_x.ai/session/update","params":`)
	_ = file.Close()
	ids, err := CompletedTurnIDs(path, "session-4")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "prompt-4" {
		t.Fatalf("unexpected completed IDs: %#v", ids)
	}
}

func TestReadTurnMapsPermissionDeniedAndConservativeSubagent(t *testing.T) {
	start := time.Now().Add(-2 * time.Second).UnixNano()
	end := time.Now().UnixNano()
	turn, ok, err := ReadTurn(Options{
		SessionID: "session-tools", TurnID: "prompt-tools", CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "delegate"}},
			{Event: "PreToolUse", RecordedNano: start + 1, Payload: map[string]any{"toolUseId": "denied", "toolName": "run_terminal_cmd", "toolInput": map[string]any{"command": "echo ok"}}},
			{Event: "PermissionDenied", RecordedNano: start + 2, Payload: map[string]any{"toolUseId": "denied", "toolName": "run_terminal_cmd"}},
			{Event: "SubagentStart", RecordedNano: start + 3, Payload: map[string]any{"subagentId": "sub-1", "subagentType": "researcher"}},
			{Event: "SubagentStop", RecordedNano: end - 1, Payload: map[string]any{"subagentId": "sub-1", "subagentType": "researcher", "stopHookActive": false}},
			{Event: "SubagentStart", RecordedNano: start + 4, Payload: map[string]any{"subagentId": "sub-blocked", "subagentType": "worker"}},
			{Event: "SubagentStop", RecordedNano: end - 2, Payload: map[string]any{"subagentId": "sub-blocked", "subagentType": "worker", "stopHookActive": true}},
			{Event: "StopCancelled", RecordedNano: end, Payload: map[string]any{"reason": "user_interrupt", "cancelledBy": "user"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.ToolCalls) != 2 {
		t.Fatalf("unpaired blocked subagent must be omitted: %#v", turn.ToolCalls)
	}
	if turn.ToolCalls[0].ErrorType != "permission_denied" || turn.ToolCalls[0].ResultStatus != "permission_denied" {
		t.Fatalf("permission denial was not mapped: %#v", turn.ToolCalls[0])
	}
	if turn.ToolCalls[1].ExtraAttributes["gen_ai.subagent.id"] != "sub-1" || turn.ToolCalls[1].ExtraAttributes["gen_ai.subagent.type"] != "researcher" {
		t.Fatalf("paired subagent lifecycle was not mapped: %#v", turn.ToolCalls[1])
	}
}

func TestReadTurnMapsToolFailureAndRejectsSkillFalsePositive(t *testing.T) {
	start := time.Now().Add(-2 * time.Second).UnixNano()
	end := time.Now().UnixNano()
	turn, ok, err := ReadTurn(Options{
		SessionID: "session-tool-failure", TurnID: "prompt-tool-failure", CaptureContent: "preview", MaxChars: 200,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "check tools"}},
			{Event: "PreToolUse", RecordedNano: start + 1, Payload: map[string]any{"toolUseId": "failed", "toolName": "run_terminal_cmd", "toolInput": map[string]any{"command": "false"}}},
			{Event: "PostToolUseFailure", RecordedNano: start + 2, Payload: map[string]any{"toolUseId": "failed", "toolName": "run_terminal_cmd", "error": "exit status 1"}},
			{Event: "PreToolUse", RecordedNano: start + 3, Payload: map[string]any{"toolUseId": "not-skill", "toolName": "read_file", "toolInput": map[string]any{"query": "/synthetic/skills/example/SKILL.md"}}},
			{Event: "PostToolUse", RecordedNano: end - 1, Payload: map[string]any{"toolUseId": "not-skill", "toolName": "read_file", "toolResult": "not a path argument"}},
			{Event: "StopCancelled", RecordedNano: end, Payload: map[string]any{"reason": "user_interrupt"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.ToolCalls) != 2 {
		t.Fatalf("unexpected tool calls: %#v", turn.ToolCalls)
	}
	if turn.ToolCalls[0].ErrorType != "tool_error" || turn.ToolCalls[0].ResultStatus != "error" || turn.ToolCalls[0].Reason != "exit status 1" {
		t.Fatalf("tool failure was not preserved: %#v", turn.ToolCalls[0])
	}
	if turn.ToolCalls[1].Skill != nil {
		t.Fatalf("a SKILL.md mention outside a path field created a Skill: %#v", turn.ToolCalls[1])
	}
}

func xaiUpdate(timestamp int, sessionID string, update map[string]any) map[string]any {
	return map[string]any{
		"timestamp": timestamp, "method": "_x.ai/session/update",
		"params": map[string]any{"sessionId": sessionID, "update": update},
	}
}

func writeUpdates(t *testing.T, path string, records []map[string]any) {
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
