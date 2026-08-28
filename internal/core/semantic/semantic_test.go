package semantic

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
)

func TestBuildProducesCanonicalTreeWithRootSummaryAndNoAssistantTokens(t *testing.T) {
	start := time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC).UnixNano()
	turn := model.Turn{
		SessionID:     "session-test",
		TurnID:        "turn-test",
		AgentRuntime:  "test-agent",
		AgentName:     "test-agent",
		StartUnixNano: start,
		EndUnixNano:   start + int64(4*time.Second),
		FinalStatus:   model.FinalStatusCompleted,
		InputPreview:  "hello",
		OutputPreview: "done",
		Usage:         model.Usage{InputTokens: 13, OutputTokens: 5},
		ExtraAttributes: map[string]any{
			"gen_ai.usage.credit": 0.45,
		},
		LLMCalls: []model.LLMCall{{
			CallID:        "llm-1",
			StartUnixNano: start,
			EndUnixNano:   start + int64(time.Second),
			RequestModel:  "model-test",
			InputPreview:  "model prompt",
			OutputPreview: "model output",
			Usage:         model.Usage{InputTokens: 13, OutputTokens: 5},
		}},
		ToolCalls: []model.ToolCall{{
			CallID:            "tool-1",
			TriggeringLLMCall: "llm-1",
			Name:              "exec",
			StartUnixNano:     start + int64(time.Second),
			EndUnixNano:       start + int64(2*time.Second),
			ResultStatus:      "completed",
			Skill:             &model.SkillUse{Name: "demo", Status: "completed", InputPreview: "skill/demo", OutputPreview: "done"},
		}},
		AssistantOutputs: []model.AssistantOutput{{
			StartUnixNano: start + int64(3*time.Second),
			EndUnixNano:   start + int64(4*time.Second),
			OutputPreview: "done",
		}},
	}

	spans := (Builder{ScopeVersion: "test"}).Build(turn)
	if len(spans) != 5 {
		t.Fatalf("expected 5 spans, got %d", len(spans))
	}
	root := spans[0]
	if root.Attributes["gen_ai.usage.input_tokens"] != int64(13) || root.Attributes["gen_ai.usage.output_tokens"] != int64(5) {
		t.Fatalf("invoke_agent aggregate usage was not preserved: %#v", root.Attributes)
	}
	assertGTraceUsage(t, root, map[string]int64{"input": 13, "output": 5, "total": 18})
	if root.Attributes["gtrace.observation.type"] != "agent" || root.Attributes["gtrace.observation.input"] != "hello" || root.Attributes["gtrace.observation.output"] != "done" {
		t.Fatalf("invoke_agent GTrace observation compatibility fields are missing: %#v", root.Attributes)
	}
	ids := map[string]string{}
	for _, span := range spans {
		ids[span.Name] = span.SpanID
		if span.Name != "invoke_agent" && span.Name != "skill:demo" && span.ParentID != root.SpanID {
			t.Fatalf("%s must be a direct root child", span.Name)
		}
		if span.Name == "assistant" {
			if _, ok := span.Attributes["gen_ai.usage.input_tokens"]; ok {
				t.Fatal("assistant must not carry token usage")
			}
		}
	}
	llm := findSpan(t, spans, "llm")
	if llm.Attributes["gen_ai.usage.input_tokens"] != int64(13) || llm.Attributes["gen_ai.usage.output_tokens"] != int64(5) {
		t.Fatalf("llm usage was not preserved: %#v", llm.Attributes)
	}
	assertGTraceUsage(t, llm, map[string]int64{"input": 13, "output": 5, "total": 18})
	if llm.Attributes["gtrace.observation.type"] != "llm" || llm.Attributes["gtrace.observation.input"] != "model prompt" || llm.Attributes["gtrace.observation.output"] != "model output" || llm.Attributes["gtrace.model.name"] != "model-test" {
		t.Fatalf("llm GTrace observation compatibility fields are missing: %#v", llm.Attributes)
	}
	if _, ok := findSpan(t, spans, "assistant").Attributes["gtrace.usage"]; ok {
		t.Fatal("assistant must not carry GTrace usage")
	}
	if findSpan(t, spans, "assistant").Attributes["gtrace.observation.type"] != "assistant" {
		t.Fatal("assistant GTrace observation type is missing")
	}
	if findSpan(t, spans, "skill:demo").ParentID != ids["tool:exec"] {
		t.Fatal("skill must be a tool child")
	}
	skill := findSpan(t, spans, "skill:demo")
	if skill.Attributes["input_preview"] != "skill/demo" || skill.Attributes["output_preview"] != "done" {
		t.Fatalf("unexpected skill previews: %#v", skill.Attributes)
	}
	if skill.Attributes["gtrace.observation.type"] != "skill" || skill.Attributes["gtrace.observation.input"] != "skill/demo" || skill.Attributes["gtrace.observation.output"] != "done" {
		t.Fatalf("skill GTrace observation compatibility fields are missing: %#v", skill.Attributes)
	}
	if _, ok := skill.Attributes["gtrace.usage"]; ok {
		t.Fatal("skill must not carry GTrace usage")
	}
	tool := findSpan(t, spans, "tool:exec")
	if tool.Attributes["triggered_by.llm_span_id"] != ids["llm"] {
		t.Fatal("tool must reference the triggering llm")
	}
	if tool.Attributes["gtrace.observation.type"] != "tool" {
		t.Fatalf("tool GTrace observation type is missing: %#v", tool.Attributes)
	}
	if _, ok := tool.Attributes["gtrace.usage"]; ok {
		t.Fatal("tool must not carry GTrace usage")
	}
}

func TestBuildExportsExplicitTurnCreditAndRootTokens(t *testing.T) {
	start := time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC).UnixNano()
	turn := model.Turn{
		SessionID:     "session-credit",
		TurnID:        "turn-credit",
		AgentRuntime:  "kiro",
		AgentName:     "Kiro",
		StartUnixNano: start,
		EndUnixNano:   start + int64(time.Second),
		FinalStatus:   model.FinalStatusCompleted,
		InputPreview:  "hello",
		Usage:         model.Usage{InputTokens: 13, OutputTokens: 5},
		CreditUsage:   0.45,
	}

	spans := (Builder{ScopeVersion: "test"}).Build(turn)
	if len(spans) != 1 {
		t.Fatalf("expected only invoke_agent, got %#v", spans)
	}
	if spans[0].Attributes["gen_ai.usage.credit"] != 0.45 {
		t.Fatalf("explicit turn credit was not preserved: %#v", spans[0].Attributes)
	}
	if spans[0].Attributes["gen_ai.usage.input_tokens"] != int64(13) || spans[0].Attributes["gen_ai.usage.output_tokens"] != int64(5) {
		t.Fatalf("invoke_agent aggregate usage was not preserved: %#v", spans[0].Attributes)
	}
}

func TestBuildSkipsUnsetAndBlankTurns(t *testing.T) {
	builder := Builder{}
	if spans := builder.Build(model.Turn{FinalStatus: model.FinalStatusUnset}); len(spans) != 0 {
		t.Fatal("unset turn must be skipped")
	}
	now := time.Now().UnixNano()
	if spans := builder.Build(model.Turn{
		StartUnixNano: now,
		EndUnixNano:   now + 1,
		FinalStatus:   model.FinalStatusCompleted,
	}); len(spans) != 0 {
		t.Fatal("blank turn must be skipped")
	}
	if spans := builder.Build(model.Turn{
		StartUnixNano: now,
		EndUnixNano:   now + 1,
		FinalStatus:   model.FinalStatusCompleted,
		Usage:         model.Usage{InputTokens: 10, OutputTokens: 2},
	}); len(spans) != 0 {
		t.Fatal("turn-level usage alone must not create an invoke_agent span")
	}
}

func TestBuildKeepsExplicitTerminalErrorWithoutContent(t *testing.T) {
	now := time.Now().UnixNano()
	spans := (Builder{}).Build(model.Turn{
		SessionID:     "session-error",
		TurnID:        "turn-error",
		StartUnixNano: now,
		EndUnixNano:   now + int64(time.Second),
		FinalStatus:   model.FinalStatusCompleted,
		ErrorType:     "dcode_agent_error",
		Reason:        "Dcode ended the session before emitting Stop",
	})
	if len(spans) != 1 || spans[0].Name != "invoke_agent" {
		t.Fatalf("explicit terminal error must build only the root span: %#v", spans)
	}
	if spans[0].Status.Code != "STATUS_CODE_ERROR" || spans[0].Attributes["error.type"] != "dcode_agent_error" {
		t.Fatalf("explicit terminal error status was not preserved: %#v", spans[0])
	}
}

func TestBuildKeepsTerminalTurnWhenContentCaptureIsDisabled(t *testing.T) {
	now := time.Now().UnixNano()
	spans := (Builder{}).Build(model.Turn{
		SessionID:     "session-private",
		TurnID:        "turn-private",
		AgentRuntime:  "grok",
		AgentName:     "Grok Build",
		StartUnixNano: now,
		EndUnixNano:   now + int64(time.Second),
		FinalStatus:   model.FinalStatusCompleted,
		InputLength:   12,
		OutputLength:  7,
		AssistantOutputs: []model.AssistantOutput{{
			StartUnixNano: now + int64(500*time.Millisecond),
			EndUnixNano:   now + int64(time.Second),
			Status:        "completed",
		}},
	})

	if len(spans) != 2 || spans[0].Name != "invoke_agent" || spans[1].Name != "assistant" {
		t.Fatalf("content-free terminal turn must preserve root and assistant spans: %#v", spans)
	}
	if spans[0].Attributes["input_length"] != 12 || spans[0].Attributes["output_length"] != 7 {
		t.Fatalf("content lengths were not preserved: %#v", spans[0].Attributes)
	}
	for _, key := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "input_preview", "output_preview"} {
		if _, exists := spans[0].Attributes[key]; exists {
			t.Fatalf("content attribute %q must stay absent: %#v", key, spans[0].Attributes)
		}
	}
}

func findSpan(t *testing.T, spans []model.Span, name string) model.Span {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("missing span %s", name)
	return model.Span{}
}

func assertGTraceUsage(t *testing.T, span model.Span, want map[string]int64) {
	t.Helper()
	raw, ok := span.Attributes["gtrace.usage"].(string)
	if !ok {
		t.Fatalf("%s is missing string gtrace.usage: %#v", span.Name, span.Attributes)
	}
	var got map[string]int64
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%s has invalid gtrace.usage: %v", span.Name, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s has unexpected gtrace.usage: got %#v want %#v", span.Name, got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s has unexpected gtrace.usage[%s]: got %d want %d", span.Name, key, got[key], value)
		}
	}
}
