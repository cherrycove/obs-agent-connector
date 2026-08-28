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

func TestClassifyPromptIDMatchesUpstreamPromptOrigins(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "prompt_origins.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		PromptID    string `json:"promptId"`
		Origin      string `json:"origin"`
		RequestType string `json:"requestType"`
		Suppressed  bool   `json:"suppressed"`
	}
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Origin, func(t *testing.T) {
			got := ClassifyPromptID(fixture.PromptID)
			if got.Name != fixture.Origin || got.RequestType != fixture.RequestType || got.Suppressed != fixture.Suppressed {
				t.Fatalf("ClassifyPromptID(%q) = %#v", fixture.PromptID, got)
			}
		})
	}
	for _, promptID := range []string{"task-completed", "Task-completed-task-1", "prefix-task-completed-task-1"} {
		if got := ClassifyPromptID(promptID); got.Name != "user" || got.Suppressed {
			t.Fatalf("non-prefix or case-mismatched ID %q was classified as synthetic: %#v", promptID, got)
		}
	}
}

func TestReadTurnSuppressesHiddenPromptOrigins(t *testing.T) {
	for _, promptID := range []string{
		"task-completed-task-1",
		"subagent-completed-agent-1",
		"workflow-completed-workflow-1",
		"notifications-notification-1",
		"goal-summary-goal-1",
		"goal-classifier-nudge-goal-1",
	} {
		t.Run(promptID, func(t *testing.T) {
			_, ok, err := ReadTurn(Options{
				SessionID: "session-origin", TurnID: promptID, CaptureContent: "preview", MaxChars: 100,
				Events: []JournalEvent{
					{Event: "UserPromptSubmit", RecordedNano: 100, Payload: map[string]any{"prompt": "runtime wake"}},
					{Event: "StopCancelled", RecordedNano: 200, Payload: map[string]any{"reason": "unknown"}},
				},
			})
			if err != nil || ok {
				t.Fatalf("hidden origin produced a turn: ok=%t err=%v", ok, err)
			}
		})
	}
}

func TestReadTurnSuppressesStructuredHiddenFutureOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		{
			"timestamp": 100,
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "session-hidden-meta",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content": map[string]any{
						"type": "text", "text": "future runtime wake",
						"_meta": map[string]any{"promptIndex": 1, "hideFromScrollback": true},
					},
				},
			},
		},
		xaiUpdate(101, "session-hidden-meta", map[string]any{
			"sessionUpdate": "turn_completed", "prompt_id": "future-runtime-origin-1",
			"stop_reason": "end_turn", "agent_result": "internal result",
		}),
	})

	hidden, ready, err := TurnHiddenFromScrollback(path, "session-hidden-meta", "future-runtime-origin-1")
	if err != nil || !ready || !hidden {
		t.Fatalf("structured visibility = hidden:%t ready:%t err:%v", hidden, ready, err)
	}
	_, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-hidden-meta", TurnID: "future-runtime-origin-1",
		CaptureContent: "preview", MaxChars: 200,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: 100, Payload: map[string]any{"prompt": "future runtime wake"}},
			{Event: "Stop", RecordedNano: 200, Payload: map[string]any{"reason": "end_turn"}},
		},
	})
	if err != nil || ok {
		t.Fatalf("structured hidden origin produced a turn: ok=%t err=%v", ok, err)
	}
}

func TestStructuredVisibilitySupportsObservedUpdateLevelMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		{
			"timestamp": 100,
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "session-update-meta",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": "runtime wake"},
					"_meta":         map[string]any{"promptIndex": 1, "hideFromScrollback": true},
				},
			},
		},
		xaiUpdate(101, "session-update-meta", map[string]any{
			"sessionUpdate": "turn_completed", "prompt_id": "future-update-meta-origin",
			"stop_reason": "end_turn", "agent_result": "internal result",
		}),
	})

	hidden, ready, err := TurnHiddenFromScrollback(path, "session-update-meta", "future-update-meta-origin")
	if err != nil || !ready || !hidden {
		t.Fatalf("observed update-level visibility = hidden:%t ready:%t err:%v", hidden, ready, err)
	}
}

func TestStructuredVisibilityKeepsInterjectFallbackAndDistinguishesNotReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		{
			"timestamp": 100,
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "session-interject",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content": map[string]any{
						"type": "text", "text": "visible interjection", "_meta": map[string]any{"promptIndex": 1},
					},
				},
			},
		},
		xaiUpdate(101, "session-interject", map[string]any{
			"sessionUpdate": "turn_completed", "prompt_id": "interject-fallback-user-1",
			"stop_reason": "end_turn", "agent_result": "visible result",
		}),
	})

	hidden, ready, err := TurnHiddenFromScrollback(path, "session-interject", "missing-turn")
	if err != nil || ready || hidden {
		t.Fatalf("unfinished visibility = hidden:%t ready:%t err:%v", hidden, ready, err)
	}
	hidden, ready, err = TurnHiddenFromScrollback(path, "session-interject", "interject-fallback-user-1")
	if err != nil || !ready || hidden {
		t.Fatalf("visible interjection = hidden:%t ready:%t err:%v", hidden, ready, err)
	}
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-interject", TurnID: "interject-fallback-user-1",
		CaptureContent: "preview", MaxChars: 200,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: 100, Payload: map[string]any{"prompt": "visible interjection"}},
			{Event: "Stop", RecordedNano: 200, Payload: map[string]any{"reason": "end_turn"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("visible interjection was dropped: ok=%t err=%v", ok, err)
	}
	if turn.ExtraAttributes["request_type"] != "user_request" || turn.ExtraAttributes["grok.prompt_origin"] != "user" {
		t.Fatalf("interject-fallback was not preserved as user: %#v", turn.ExtraAttributes)
	}
}

func TestStructuredVisibilityIgnoresHiddenInTurnMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		{
			"timestamp": 100,
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "session-in-turn",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content": map[string]any{
						"type": "text", "text": "visible user prompt", "_meta": map[string]any{"promptIndex": 1},
					},
				},
			},
		},
		xaiUpdate(101, "session-in-turn", map[string]any{
			"sessionUpdate": "response_started", "message_id": "message-1", "model": "grok-code",
		}),
		{
			"timestamp": 102,
			"method":    "session/update",
			"params": map[string]any{
				"sessionId": "session-in-turn",
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content": map[string]any{
						"type": "text", "text": "runtime reminder",
						"_meta": map[string]any{"promptIndex": 2, "hideFromScrollback": true},
					},
				},
			},
		},
		xaiUpdate(103, "session-in-turn", map[string]any{
			"sessionUpdate": "response_completed", "message_id": "message-1",
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 5, "output_tokens": 2},
		}),
		xaiUpdate(104, "session-in-turn", map[string]any{
			"sessionUpdate": "turn_completed", "prompt_id": "ordinary-user-turn",
			"stop_reason": "end_turn", "agent_result": "visible result",
		}),
	})

	completed, err := CompletedTurns(path, "session-in-turn")
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].TurnID != "ordinary-user-turn" || completed[0].HiddenFromScrollback {
		t.Fatalf("mid-turn hidden message reclassified the root: %#v", completed)
	}
	hidden, ready, err := TurnHiddenFromScrollback(path, "session-in-turn", "ordinary-user-turn")
	if err != nil || !ready || hidden {
		t.Fatalf("root visibility = hidden:%t ready:%t err:%v", hidden, ready, err)
	}
}

func TestReadTurnDoesNotSuppressSystemReminderTextWithoutOriginPrefix(t *testing.T) {
	turn, ok, err := ReadTurn(Options{
		SessionID: "session-origin", TurnID: "ordinary-user-turn", CaptureContent: "preview", MaxChars: 200,
		Events: []JournalEvent{
			{Event: "UserPromptSubmit", RecordedNano: 100, Payload: map[string]any{"prompt": "<system-reminder>literal user text</system-reminder>"}},
			{Event: "StopCancelled", RecordedNano: 200, Payload: map[string]any{"reason": "unknown"}},
		},
	})
	if err != nil || !ok {
		t.Fatalf("text-only heuristic suppressed a user turn: ok=%t err=%v", ok, err)
	}
	if turn.ExtraAttributes["request_type"] != "user_request" || turn.ExtraAttributes["grok.prompt_origin"] != "user" {
		t.Fatalf("ordinary prompt ID was not kept as a user request: %#v", turn.ExtraAttributes)
	}
}

func TestReadTurnClassifiesObservableSyntheticOrigins(t *testing.T) {
	for _, test := range []struct {
		promptID    string
		requestType string
		origin      string
	}{
		{promptID: "scheduler-fired-schedule-1", requestType: "scheduled_task", origin: "scheduler_fired"},
		{promptID: "plan-resume-plan-1", requestType: "plan_resume", origin: "plan_resume"},
	} {
		t.Run(test.origin, func(t *testing.T) {
			promptPayload := map[string]any{"prompt": "observable runtime turn"}
			turn, ok, err := ReadTurn(Options{
				SessionID: "session-origin", TurnID: test.promptID, CaptureContent: "preview", MaxChars: 100,
				Events: []JournalEvent{
					{Event: "UserPromptSubmit", RecordedNano: 100, Payload: promptPayload},
					{Event: "StopCancelled", RecordedNano: 200, Payload: map[string]any{"reason": "unknown"}},
				},
			})
			if err != nil || !ok {
				t.Fatalf("observable origin was dropped: ok=%t err=%v", ok, err)
			}
			if turn.ExtraAttributes["request_type"] != test.requestType || turn.ExtraAttributes["grok.prompt_origin"] != test.origin {
				t.Fatalf("unexpected prompt-origin attributes: %#v", turn.ExtraAttributes)
			}
		})
	}
}

func TestParentMessageClassifierDoesNotInventMissingLifecycle(t *testing.T) {
	origin := ClassifyPromptID("parent-message-message-1")
	if origin.Name != "parent_agent_message" || origin.RequestType != "subagent" || origin.Suppressed {
		t.Fatalf("unexpected parent-message classification: %#v", origin)
	}
	_, ok, err := ReadTurn(Options{
		SessionID: "session-parent", TurnID: "parent-message-message-1",
		CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{
			{Event: "SubagentStop", RecordedNano: 200, Payload: map[string]any{"subagentId": "child-1"}},
		},
	})
	if err != nil || ok {
		t.Fatalf("missing UserPromptSubmit invented a parent-message turn: ok=%t err=%v", ok, err)
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
			{Event: "PreToolUse", RecordedNano: start + 2500*int64(time.Millisecond), Payload: map[string]any{"toolUseId": "tool-1", "toolName": "read_file", "toolInput": map[string]any{"file_path": "/synthetic/skills/example/SKILL.md"}}},
			{Event: "PostToolUse", RecordedNano: start + 2750*int64(time.Millisecond), Payload: map[string]any{"toolUseId": "tool-1", "toolName": "read_file", "toolResult": "contents", "durationMs": 250}},
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
	if turn.ToolCalls[0].TriggeringLLMCall != "message-1" {
		t.Fatalf("exact response did not trigger the tool: %#v", turn.ToolCalls[0])
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
	if spans[0].Attributes["gen_ai.usage.input_tokens"] != int64(35) || spans[0].Attributes["gen_ai.usage.output_tokens"] != int64(12) {
		t.Fatalf("root aggregate usage was not preserved: %#v", spans[0].Attributes)
	}
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
			if span.Attributes["triggered_by.llm_span_id"] != spans[1].SpanID {
				t.Fatalf("tool did not reference its exact response span: %#v", span)
			}
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

func TestExactResponseToolCorrelationRequiresCompleteCausalGap(t *testing.T) {
	calls := []model.LLMCall{
		{CallID: "llm-tool", EndUnixNano: 20, FinishReasons: []string{"tool_use"}},
		{CallID: "llm-final", StartUnixNano: 40, EndUnixNano: 50, FinishReasons: []string{"end_turn"}},
	}
	tools := []toolBoundary{
		{ID: "valid", Pre: JournalEvent{RecordedNano: 21}, Post: JournalEvent{RecordedNano: 39}},
		{ID: "incomplete", Pre: JournalEvent{RecordedNano: 21}},
		{ID: "before-response-end", Pre: JournalEvent{RecordedNano: 19}, Post: JournalEvent{RecordedNano: 30}},
		{ID: "after-next-response", Pre: JournalEvent{RecordedNano: 21}, Post: JournalEvent{RecordedNano: 41}},
	}

	triggers := toolTriggersFromExactResponses(calls, tools, 60)
	if len(triggers) != 1 || triggers["valid"] != "llm-tool" {
		t.Fatalf("exact response correlation accepted incomplete or ambiguous boundaries: %#v", triggers)
	}
}

func TestSessionEventLoopBoundaryDoesNotInventToolFinishReason(t *testing.T) {
	selected := &sessionTurnBlock{
		StartUnixNano: 100,
		EndUnixNano:   600,
		SessionID:     "session-events",
		ModelID:       "grok-test",
		Events: []sessionEventRecord{
			{Type: "loop_started", LoopIndex: 0, UnixNano: 110},
			{Type: "phase_changed", Phase: "waiting_for_model", UnixNano: 120},
			{Type: "loop_started", LoopIndex: 1, UnixNano: 300},
			{Type: "phase_changed", Phase: "waiting_for_model", UnixNano: 310},
			{Type: "turn_ended", Outcome: "completed", UnixNano: 600},
		},
	}
	record := transcriptRecord{}
	record.Params.Update = map[string]any{
		"stop_reason": "end_turn",
		"usage":       map[string]any{"modelCalls": 2},
	}

	calls, _, ok := callsFromSessionEvents(selected, record, "turn-events", 100, 600, nil)
	if !ok || len(calls) != 2 {
		t.Fatalf("session event calls were not reconstructed: ok=%t calls=%#v", ok, calls)
	}
	if len(calls[0].FinishReasons) != 0 || len(calls[1].FinishReasons) != 1 || calls[1].FinishReasons[0] != "end_turn" {
		t.Fatalf("loop boundary invented a tool finish reason: %#v", calls)
	}
}

func TestMatchingSessionTurnBlockRequiresUniqueBoundedEndpoints(t *testing.T) {
	hookStart := int64(10 * time.Second)
	hookEnd := int64(20 * time.Second)
	matching := sessionTurnBlock{
		StartUnixNano: hookStart - int64(maxSessionEventHookSkew),
		EndUnixNano:   hookEnd + int64(maxSessionEventHookSkew),
		SessionID:     "session-match",
	}

	if selected, ok := matchingSessionTurnBlock([]sessionTurnBlock{matching}, "session-match", hookStart, hookEnd); !ok || selected == nil || selected.StartUnixNano != matching.StartUnixNano || selected.EndUnixNano != matching.EndUnixNano {
		t.Fatalf("inclusive skew boundary did not match: ok=%t selected=%#v", ok, selected)
	}
	outOfBounds := matching
	outOfBounds.StartUnixNano--
	if selected, ok := matchingSessionTurnBlock([]sessionTurnBlock{outOfBounds}, "session-match", hookStart, hookEnd); ok || selected != nil {
		t.Fatalf("out-of-bounds event turn matched: %#v", selected)
	}
	if selected, ok := matchingSessionTurnBlock([]sessionTurnBlock{matching, matching}, "session-match", hookStart, hookEnd); ok || selected != nil {
		t.Fatalf("ambiguous event turns matched: %#v", selected)
	}
}

func TestReadTurnBuildsSingleCallFromStableTurnUsage(t *testing.T) {
	path := filepath.Join("testdata", "updates_turn_usage_single.jsonl")
	start := time.Unix(393, 0).UnixNano()
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "synthetic-turn-usage-session", TurnID: "synthetic-turn-usage-prompt",
		CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{{Event: "UserPromptSubmit", RecordedNano: start, Payload: map[string]any{"prompt": "hello"}}},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if turn.Usage.InputTokens != 17991 || turn.Usage.OutputTokens != 221 || turn.Usage.ReasoningTokens != 209 {
		t.Fatalf("turn aggregate usage was not preserved: %#v", turn.Usage)
	}
	if len(turn.LLMCalls) != 1 {
		t.Fatalf("stable single-call usage did not create one LLM call: %#v", turn.LLMCalls)
	}
	call := turn.LLMCalls[0]
	if call.ResponseModel != "grok-4.6" || call.Usage != turn.Usage {
		t.Fatalf("single-call model or usage was not preserved: %#v", call)
	}
	if call.InputMessages == nil || call.OutputMessages == nil || call.InputPreview != "hello" || call.OutputPreview != "synthetic answer" || call.OutputKind != "text" {
		t.Fatalf("single-call turn content was not attached to the LLM call: %#v", call)
	}
	if call.EndUnixNano-call.StartUnixNano != int64(5911*time.Millisecond) || call.ExtraAttributes["timing.source"] != "grok_turn_completed_usage" {
		t.Fatalf("single-call timing was not derived from apiDurationMs: %#v", call)
	}

	spans := (semantic.Builder{ScopeName: "gtrace-grok-test", ScopeVersion: "test"}).Build(turn)
	if len(spans) != 3 || spans[0].Name != "invoke_agent" || spans[1].Name != "llm" || spans[2].Name != "assistant" {
		t.Fatalf("unexpected single-call trace shape: %#v", spans)
	}
	for _, span := range spans[:2] {
		if span.Attributes["gen_ai.usage.input_tokens"] != int64(17991) || span.Attributes["gen_ai.usage.output_tokens"] != int64(221) {
			t.Fatalf("token usage missing from %s: %#v", span.Name, span.Attributes)
		}
	}
	if spans[0].Attributes["usage_input_tokens"] != int64(17991) || spans[0].Attributes["usage_output_tokens"] != int64(221) {
		t.Fatalf("root token compatibility aliases are missing: %#v", spans[0].Attributes)
	}
	if spans[1].Attributes["gen_ai.input.messages"] == nil || spans[1].Attributes["gen_ai.output.messages"] == nil || spans[1].Attributes["usage_input_tokens"] != nil {
		t.Fatalf("single LLM content or root-only alias policy is incorrect: %#v", spans[1].Attributes)
	}
	tokenPoints := 0
	for _, metric := range coremetrics.Build(spans) {
		if metric.Name == "gen_ai.client.token.usage" {
			tokenPoints++
		}
	}
	if tokenPoints != 2 {
		t.Fatalf("expected one input and one output token metric, got %d", tokenPoints)
	}
}

func TestReadTurnEnrichesSingleEventCallWithTurnContentAndUsage(t *testing.T) {
	tempDir := t.TempDir()
	transcriptPath := filepath.Join(tempDir, "updates.jsonl")
	writeUpdates(t, transcriptPath, []map[string]any{xaiUpdate(610, "single-event-session", map[string]any{
		"sessionUpdate": "turn_completed", "prompt_id": "single-event-prompt", "stop_reason": "end_turn", "agent_result": "answer",
		"usage": map[string]any{
			"inputTokens": 10, "outputTokens": 4, "cachedReadTokens": 3, "reasoningTokens": 2, "modelCalls": 1,
			"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 1}},
		},
	})})
	events := strings.Join([]string{
		`{"ts":"1970-01-01T00:10:00.100Z","type":"turn_started","session_id":"single-event-session","model_id":"grok-4.6","schema_version":"1.0"}`,
		`{"ts":"1970-01-01T00:10:00.200Z","type":"loop_started","loop_index":0}`,
		`{"ts":"1970-01-01T00:10:00.300Z","type":"phase_changed","phase":"waiting_for_model"}`,
		`{"ts":"1970-01-01T00:10:01.000Z","type":"first_token"}`,
		`{"ts":"1970-01-01T00:10:10.000Z","type":"turn_ended","outcome":"completed"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(tempDir, "events.jsonl"), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}

	turn, ok, err := ReadTurn(Options{
		TranscriptPath: transcriptPath,
		SessionID:      "single-event-session",
		TurnID:         "single-event-prompt",
		CaptureContent: "preview",
		MaxChars:       1_000,
		Events: []JournalEvent{{
			Event: "UserPromptSubmit", RecordedNano: mustRFC3339Nano(t, "1970-01-01T00:10:00.200Z"),
			Payload: map[string]any{"prompt": "hello"},
		}},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.LLMCalls) != 1 {
		t.Fatalf("single event turn did not produce one LLM call: %#v", turn.LLMCalls)
	}
	call := turn.LLMCalls[0]
	if call.ExtraAttributes["timing.source"] != "grok_events" || call.Usage != turn.Usage || call.InputPreview != "hello" || call.OutputPreview != "answer" {
		t.Fatalf("single event call was not enriched from unambiguous turn evidence: %#v", call)
	}
	spans := (semantic.Builder{}).Build(turn)
	if spans[1].Attributes["gen_ai.input.messages"] == nil || spans[1].Attributes["gen_ai.output.messages"] == nil || spans[1].Attributes["gen_ai.usage.input_tokens"] != int64(10) || spans[1].Attributes["gen_ai.usage.output_tokens"] != int64(4) {
		t.Fatalf("single event LLM span lacks content or usage: %#v", spans[1].Attributes)
	}
	tokenPoints := 0
	for _, metric := range coremetrics.Build(spans) {
		if metric.Name == "gen_ai.client.token.usage" {
			tokenPoints++
		}
	}
	if tokenPoints != 2 {
		t.Fatalf("expected input/output token metrics for one proven call, got %d", tokenPoints)
	}
}

func TestReadTurnDoesNotCopyMultiCallAggregateToLLM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updates.jsonl")
	writeUpdates(t, path, []map[string]any{
		xaiUpdate(410, "session-multi-usage", map[string]any{
			"sessionUpdate": "turn_completed", "prompt_id": "prompt-multi-usage", "stop_reason": "end_turn", "agent_result": "answer",
			"usage": map[string]any{
				"inputTokens": 30, "outputTokens": 8, "modelCalls": 2, "apiDurationMs": 3000,
				"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 2}},
			},
		}),
	})
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "session-multi-usage", TurnID: "prompt-multi-usage", CaptureContent: "preview", MaxChars: 100,
		Events: []JournalEvent{{Event: "UserPromptSubmit", RecordedNano: time.Unix(409, 0).UnixNano(), Payload: map[string]any{"prompt": "hello"}}},
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.LLMCalls) != 0 {
		t.Fatalf("multi-call aggregate was copied to an invented LLM call: %#v", turn.LLMCalls)
	}
	spans := (semantic.Builder{}).Build(turn)
	if len(spans) != 2 || spans[0].Attributes["gen_ai.usage.input_tokens"] != int64(30) {
		t.Fatalf("multi-call root aggregate was not preserved: %#v", spans)
	}
	for _, metric := range coremetrics.Build(spans) {
		if metric.Name == "gen_ai.client.token.usage" {
			t.Fatalf("multi-call aggregate created an unproven token metric: %#v", metric)
		}
	}
}

func TestReadTurnBuildsCallsFromVersionedSessionEvents(t *testing.T) {
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: filepath.Join("testdata", "updates_turn_usage_multi_tools.jsonl"),
		SessionID:      "synthetic-events-session",
		TurnID:         "synthetic-events-prompt",
		CaptureContent: "preview",
		MaxChars:       1_000,
		Events:         readJournalFixture(t, filepath.Join("testdata", "journal_turn_usage_multi_tools.jsonl")),
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.LLMCalls) != 2 {
		t.Fatalf("session events did not produce two calls: %#v", turn.LLMCalls)
	}
	firstStart := mustRFC3339Nano(t, "2026-08-27T03:27:08.771Z")
	firstEnd := mustRFC3339Nano(t, "2026-08-27T03:27:20.639Z")
	secondStart := mustRFC3339Nano(t, "2026-08-27T03:27:45.563Z")
	secondEnd := mustRFC3339Nano(t, "2026-08-27T03:28:03.172Z")
	eventTurnStart := mustRFC3339Nano(t, "2026-08-27T03:27:08.347Z")
	if turn.LLMCalls[0].StartUnixNano != firstStart || turn.LLMCalls[0].EndUnixNano != firstEnd ||
		turn.LLMCalls[1].StartUnixNano != secondStart || turn.LLMCalls[1].EndUnixNano != secondEnd {
		t.Fatalf("session event boundaries were not preserved: %#v", turn.LLMCalls)
	}
	if turn.StartUnixNano != eventTurnStart || turn.EndUnixNano != secondEnd || turn.ExtraAttributes["timing.source"] != "grok_events_and_hooks" {
		t.Fatalf("root did not expand across bounded Hook delivery skew: %#v", turn)
	}
	for _, call := range turn.LLMCalls {
		if call.RequestModel != "grok-4.6" || call.Usage != (model.Usage{}) || call.ExtraAttributes["timing.source"] != "grok_events" {
			t.Fatalf("unexpected event-derived LLM call: %#v", call)
		}
		if call.ExtraAttributes["gtrace.synthetic"] != nil || call.ExtraAttributes["gtrace.timing.estimated"] != nil {
			t.Fatalf("real event boundary was marked synthetic: %#v", call.ExtraAttributes)
		}
	}
	if turn.LLMCalls[0].TTFTMs != 0 || turn.LLMCalls[1].TTFTMs != 10909 {
		t.Fatalf("first-token timing was not attached to its call: %#v", turn.LLMCalls)
	}
	if turn.Usage.InputTokens != 38315 || turn.Usage.OutputTokens != 155 || turn.Usage.ReasoningTokens != 61 {
		t.Fatalf("aggregate usage was not retained on the root: %#v", turn.Usage)
	}
	if turn.LLMCalls[0].InputMessages == nil || turn.LLMCalls[0].OutputMessages == nil || turn.LLMCalls[0].OutputKind != "tool_call" {
		t.Fatalf("first LLM call was not enriched from chat history: %#v", turn.LLMCalls[0])
	}
	if turn.LLMCalls[1].InputMessages == nil || turn.LLMCalls[1].OutputMessages == nil || turn.LLMCalls[1].OutputKind != "text" {
		t.Fatalf("second LLM call was not enriched from chat history: %#v", turn.LLMCalls[1])
	}
	if turn.LLMCalls[0].ExtraAttributes["content.source"] != "grok_chat_history" || turn.LLMCalls[1].ExtraAttributes["content.source"] != "grok_chat_history" {
		t.Fatalf("chat history evidence was not marked: %#v", turn.LLMCalls)
	}
	if len(turn.AssistantOutputs) != 1 {
		t.Fatalf("tool-call-only response created an assistant or terminal output was duplicated: %#v", turn.AssistantOutputs)
	}
	assistant := turn.AssistantOutputs[0]
	if assistant.OutputPreview != "Synthetic answer." || assistant.ResponseModel != "grok-4.6" ||
		assistant.ExtraAttributes["content.source"] != "grok_chat_history" || assistant.ExtraAttributes["timing.source"] != "grok_llm_boundary" {
		t.Fatalf("final visible assistant was not enriched from its LLM response: %#v", assistant)
	}
	encodedMessages, err := json.Marshal(turn.LLMCalls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedMessages), "fixture-only") || !strings.Contains(string(encodedMessages), "[REDACTED]") {
		t.Fatalf("chat history tool arguments were not recursively sanitized: %s", encodedMessages)
	}
	for _, tool := range turn.ToolCalls {
		if tool.TriggeringLLMCall != turn.LLMCalls[0].CallID {
			t.Fatalf("parallel tool was not associated with the preceding LLM: %#v", tool)
		}
	}

	spans := (semantic.Builder{ScopeName: "gtrace-grok-test", ScopeVersion: "test"}).Build(turn)
	if len(spans) != 7 || spans[0].Name != "invoke_agent" || spans[1].Name != "llm" || spans[2].Name != "llm" || spans[6].Name != "assistant" {
		t.Fatalf("unexpected event-derived span tree: %#v", spans)
	}
	if spans[1].Attributes["gen_ai.input.messages"] == nil || spans[1].Attributes["gen_ai.output.messages"] == nil || spans[1].Attributes["output_kind"] != "tool_call" ||
		spans[2].Attributes["gen_ai.input.messages"] == nil || spans[2].Attributes["gen_ai.output.messages"] == nil || spans[2].Attributes["output_kind"] != "text" {
		t.Fatalf("chat history content was not emitted on both LLM spans: %#v %#v", spans[1].Attributes, spans[2].Attributes)
	}
	var gtraceUsage map[string]int64
	rawGTraceUsage, ok := spans[0].Attributes["gtrace.usage"].(string)
	if !ok {
		t.Fatalf("root GTrace usage compatibility payload is missing: %#v", spans[0].Attributes)
	}
	if err := json.Unmarshal([]byte(rawGTraceUsage), &gtraceUsage); err != nil {
		t.Fatalf("root GTrace usage compatibility payload is invalid: %v (%#v)", err, spans[0].Attributes)
	}
	if gtraceUsage["input"] != 38315 || gtraceUsage["output"] != 155 || gtraceUsage["total"] != 38470 {
		t.Fatalf("root GTrace usage compatibility payload is incomplete: %#v", gtraceUsage)
	}
	for _, llm := range spans[1:3] {
		if llm.Attributes["gtrace.observation.type"] != "llm" || llm.Attributes["gtrace.observation.input"] == nil || llm.Attributes["gtrace.observation.output"] == nil {
			t.Fatalf("LLM GTrace input/output compatibility fields are missing: %#v", llm.Attributes)
		}
		if llm.Attributes["gtrace.usage"] != nil {
			t.Fatalf("turn aggregate usage was copied to an individual LLM: %#v", llm.Attributes)
		}
	}
	if spans[6].Attributes["gtrace.observation.type"] != "assistant" || spans[6].Attributes["gtrace.observation.output"] != "Synthetic answer." {
		t.Fatalf("assistant GTrace output compatibility fields are missing: %#v", spans[6].Attributes)
	}
	rootID := spans[0].SpanID
	triggerID := spans[1].SpanID
	for _, span := range spans[1:] {
		if span.ParentID != rootID {
			t.Fatalf("%s was not a direct root child: %#v", span.Name, span)
		}
		if strings.HasPrefix(span.Name, "tool:") && span.Attributes["triggered_by.llm_span_id"] != triggerID {
			t.Fatalf("tool span lacked the event-derived LLM association: %#v", span)
		}
	}
	for _, metric := range coremetrics.Build(spans) {
		if metric.Name == "gen_ai.client.token.usage" {
			t.Fatalf("root aggregate tokens leaked into an event-derived per-call metric: %#v", metric)
		}
	}
}

func TestReadTurnEmitsIntermediateVisibleChatHistoryAssistant(t *testing.T) {
	tempDir := t.TempDir()
	fixtures := map[string]string{
		"updates_turn_usage_multi_tools.jsonl": "updates.jsonl",
		"events.jsonl":                         "events.jsonl",
		"chat_history_intermediate_text.jsonl": "chat_history.jsonl",
	}
	for source, destination := range fixtures {
		body, err := os.ReadFile(filepath.Join("testdata", source))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tempDir, destination), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	turn, ok, err := ReadTurn(Options{
		TranscriptPath: filepath.Join(tempDir, "updates.jsonl"),
		SessionID:      "synthetic-events-session",
		TurnID:         "synthetic-events-prompt",
		CaptureContent: "preview",
		MaxChars:       1_000,
		Events:         readJournalFixture(t, filepath.Join("testdata", "journal_turn_usage_multi_tools.jsonl")),
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.AssistantOutputs) != 2 {
		t.Fatalf("expected one intermediate and one terminal assistant, got %#v", turn.AssistantOutputs)
	}
	first := turn.AssistantOutputs[0]
	if first.OutputPreview != "I will check the synthetic sources." || first.ResponseModel != "grok-4.6" ||
		first.StartUnixNano != turn.LLMCalls[0].EndUnixNano || first.EndUnixNano != first.StartUnixNano+1 {
		t.Fatalf("intermediate visible assistant lacked matching content, model, or timing: %#v", first)
	}
	final := turn.AssistantOutputs[1]
	if final.OutputPreview != "Synthetic answer." || final.ResponseModel != "grok-4.6" {
		t.Fatalf("terminal assistant was duplicated or lost its persisted evidence: %#v", turn.AssistantOutputs)
	}

	spans := (semantic.Builder{}).Build(turn)
	assistantSpans := 0
	for _, span := range spans {
		if span.Name != "assistant" {
			continue
		}
		assistantSpans++
		if span.Attributes["gen_ai.usage.input_tokens"] != nil || span.Attributes["gen_ai.usage.output_tokens"] != nil {
			t.Fatalf("assistant span carried fabricated token usage: %#v", span.Attributes)
		}
	}
	if assistantSpans != 2 {
		t.Fatalf("expected two assistant spans, got %d: %#v", assistantSpans, spans)
	}
}

func TestEnrichAssistantOutputsDoesNotOverwriteDifferentTerminalOutput(t *testing.T) {
	turn := model.Turn{
		StartUnixNano: 100,
		EndUnixNano:   1_000,
		LLMCalls: []model.LLMCall{
			{StartUnixNano: 200, EndUnixNano: 300, ResponseModel: "grok-test", Status: "ok"},
			{StartUnixNano: 400, EndUnixNano: 500, ResponseModel: "grok-test", Status: "ok"},
		},
		AssistantOutputs: []model.AssistantOutput{{
			StartUnixNano: 999,
			EndUnixNano:   1_000,
			OutputPreview: "Terminal output supplied by the completion event.",
			OutputKind:    "text",
			Status:        "ok",
		}},
	}
	calls := []chatHistoryCall{
		{
			AssistantOutputMessages: []any{map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": "Intermediate note."}}}},
			AssistantOutputPreview:  "Intermediate note.",
		},
		{},
	}

	enrichAssistantOutputs(&turn, calls)
	if len(turn.AssistantOutputs) != 2 {
		t.Fatalf("expected distinct intermediate and terminal assistants, got %#v", turn.AssistantOutputs)
	}
	if turn.AssistantOutputs[0].OutputPreview != "Intermediate note." || turn.AssistantOutputs[1].OutputPreview != "Terminal output supplied by the completion event." {
		t.Fatalf("terminal assistant was overwritten by an unrelated intermediate response: %#v", turn.AssistantOutputs)
	}
}

func TestChatAssistantMessageSanitizesVisibleText(t *testing.T) {
	_, assistantOutput, assistantPreview, _ := chatAssistantMessage(chatHistoryItem{
		Content: "password=fixture-only-secret Synthetic answer.",
	}, 1_000)
	encoded, err := json.Marshal(assistantOutput)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture-only-secret") || strings.Contains(assistantPreview, "fixture-only-secret") {
		t.Fatalf("assistant content was not sanitized: messages=%s preview=%q", encoded, assistantPreview)
	}
	if !strings.Contains(string(encoded), "[REDACTED]") || !strings.Contains(assistantPreview, "[REDACTED]") {
		t.Fatalf("assistant redaction marker is missing: messages=%s preview=%q", encoded, assistantPreview)
	}
}

func TestReadTurnSynthesizesCallsFromCompleteHookClusters(t *testing.T) {
	tempDir := t.TempDir()
	updates, err := os.ReadFile(filepath.Join("testdata", "updates_turn_usage_multi_tools.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(tempDir, "updates.jsonl")
	if err := os.WriteFile(transcriptPath, updates, 0o600); err != nil {
		t.Fatal(err)
	}
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: transcriptPath,
		SessionID:      "synthetic-events-session",
		TurnID:         "synthetic-events-prompt",
		CaptureContent: "preview",
		MaxChars:       1_000,
		Events:         readJournalFixture(t, filepath.Join("testdata", "journal_turn_usage_multi_tools.jsonl")),
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.LLMCalls) != 2 {
		t.Fatalf("hook cluster fallback did not produce two calls: %#v", turn.LLMCalls)
	}
	totalDuration := int64(0)
	for _, call := range turn.LLMCalls {
		totalDuration += call.EndUnixNano - call.StartUnixNano
		if call.ResponseModel != "grok-4.6" || call.Usage != (model.Usage{}) {
			t.Fatalf("aggregate model or tokens were mapped incorrectly: %#v", call)
		}
		if call.ExtraAttributes["timing.source"] != "grok_hook_boundaries" || call.ExtraAttributes["gtrace.synthetic"] != true || call.ExtraAttributes["gtrace.timing.estimated"] != true {
			t.Fatalf("synthetic timing markers are incomplete: %#v", call.ExtraAttributes)
		}
	}
	if totalDuration != int64(29385*time.Millisecond) {
		t.Fatalf("estimated calls total %s, want 29.385s", time.Duration(totalDuration))
	}
	for _, tool := range turn.ToolCalls {
		if tool.TriggeringLLMCall != turn.LLMCalls[0].CallID {
			t.Fatalf("parallel tool was not associated with the preceding synthetic LLM: %#v", tool)
		}
	}

	spans := (semantic.Builder{}).Build(turn)
	if len(spans) != 7 || spans[0].Name != "invoke_agent" || spans[1].Name != "llm" || spans[2].Name != "llm" || spans[6].Name != "assistant" {
		t.Fatalf("unexpected synthetic span tree: %#v", spans)
	}
	rootID := spans[0].SpanID
	triggerID := spans[1].SpanID
	for _, span := range spans[1:] {
		if span.ParentID != rootID {
			t.Fatalf("%s was not a direct root child: %#v", span.Name, span)
		}
		if strings.HasPrefix(span.Name, "tool:") && span.Attributes["triggered_by.llm_span_id"] != triggerID {
			t.Fatalf("tool span lacked the synthetic LLM association: %#v", span)
		}
	}
	for _, metric := range coremetrics.Build(spans) {
		if metric.Name == "gen_ai.client.token.usage" {
			t.Fatalf("aggregate turn tokens were split into synthetic call metrics: %#v", metric)
		}
	}
}

func TestReadTurnRejectsMismatchedHookClusterEvidence(t *testing.T) {
	baseEvents := readJournalFixture(t, filepath.Join("testdata", "journal_turn_usage_multi_tools.jsonl"))
	validUsage := func() map[string]any {
		return map[string]any{
			"inputTokens": 38315, "outputTokens": 155, "modelCalls": 2, "apiDurationMs": 29385,
			"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 2}},
		}
	}
	tests := []struct {
		name   string
		usage  map[string]any
		events func([]JournalEvent) []JournalEvent
	}{
		{
			name: "model usage call count",
			usage: map[string]any{
				"inputTokens": 38315, "outputTokens": 155, "modelCalls": 2, "apiDurationMs": 29385,
				"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 3}},
			},
		},
		{
			name: "tool cluster count",
			usage: map[string]any{
				"inputTokens": 38315, "outputTokens": 155, "modelCalls": 3, "apiDurationMs": 29385,
				"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 3}},
			},
		},
		{
			name: "aggregate duration exceeds causal gaps",
			usage: map[string]any{
				"inputTokens": 38315, "outputTokens": 155, "modelCalls": 2, "apiDurationMs": 30129,
				"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 2}},
			},
		},
		{
			name:  "incomplete tool interval",
			usage: validUsage(),
			events: func(events []JournalEvent) []JournalEvent {
				filtered := make([]JournalEvent, 0, len(events))
				for _, event := range events {
					if event.Event == "PostToolUse" && eventToolID(event.Payload) == "synthetic-search-3" {
						continue
					}
					filtered = append(filtered, event)
				}
				return filtered
			},
		},
		{
			name: "incomplete aggregate usage",
			usage: map[string]any{
				"inputTokens": 38315, "outputTokens": 155, "modelCalls": 2, "apiDurationMs": 29385, "usageIsIncomplete": true,
				"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 2}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "updates.jsonl")
			writeUpdates(t, path, []map[string]any{xaiUpdate(1787801283, "synthetic-events-session", map[string]any{
				"sessionUpdate": "turn_completed", "prompt_id": "synthetic-events-prompt", "stop_reason": "end_turn", "agent_result": "Synthetic answer.", "usage": test.usage,
			})})
			events := append([]JournalEvent(nil), baseEvents...)
			if test.events != nil {
				events = test.events(events)
			}
			turn, ok, err := ReadTurn(Options{
				TranscriptPath: path, SessionID: "synthetic-events-session", TurnID: "synthetic-events-prompt",
				CaptureContent: "preview", MaxChars: 1_000, Events: events,
			})
			if err != nil || !ok {
				t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
			}
			if len(turn.LLMCalls) != 0 {
				t.Fatalf("mismatched evidence fabricated LLM calls: %#v", turn.LLMCalls)
			}
		})
	}
}

func TestReadTurnRejectsSessionEventsWhenAggregateCallCountDiffers(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "updates.jsonl")
	writeUpdates(t, path, []map[string]any{xaiUpdate(1787801283, "synthetic-events-session", map[string]any{
		"sessionUpdate": "turn_completed", "prompt_id": "synthetic-events-prompt", "stop_reason": "end_turn", "agent_result": "Synthetic answer.",
		"usage": map[string]any{
			"inputTokens": 38315, "outputTokens": 155, "modelCalls": 3, "apiDurationMs": 29385,
			"modelUsage": map[string]any{"grok-4.6": map[string]any{"modelCalls": 3}},
		},
	})})
	events, err := os.ReadFile(filepath.Join("testdata", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "events.jsonl"), events, 0o600); err != nil {
		t.Fatal(err)
	}
	turn, ok, err := ReadTurn(Options{
		TranscriptPath: path, SessionID: "synthetic-events-session", TurnID: "synthetic-events-prompt",
		CaptureContent: "preview", MaxChars: 1_000,
		Events: readJournalFixture(t, filepath.Join("testdata", "journal_turn_usage_multi_tools.jsonl")),
	})
	if err != nil || !ok {
		t.Fatalf("ReadTurn() ok=%t err=%v", ok, err)
	}
	if len(turn.LLMCalls) != 0 {
		t.Fatalf("event/aggregate count mismatch fabricated LLM calls: %#v", turn.LLMCalls)
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

func readJournalFixture(t *testing.T, path string) []JournalEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	events := make([]JournalEvent, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event JournalEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func mustRFC3339Nano(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UnixNano()
}
