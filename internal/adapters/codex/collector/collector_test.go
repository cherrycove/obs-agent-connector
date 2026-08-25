package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/buildinfo"
	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/config"
	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/util"
)

func TestCollectRolloutBuildsRootLLMAssistantToolAndSkillSpans(t *testing.T) {
	home := t.TempDir()
	userSkillDir := filepath.Join(home, ".codex", "skills", "dashboard")
	if err := os.MkdirAll(userSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userSkillFile := filepath.Join(userSkillDir, "SKILL.md")
	if err := os.WriteFile(userSkillFile, []byte(`---
name: dashboard
description: Generate an observability dashboard.
version: 1.4.0
---

Generate an observability dashboard.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rollout := filepath.Join(home, "rollout.jsonl")
	body := []byte(joinJSONLines(
		row("2026-06-03T10:00:00.000Z", "session_meta", map[string]any{
			"id":             "sess-skill-order",
			"cli_version":    "0.140.0",
			"model_provider": "openai",
			"timestamp":      "2026-06-03T09:59:58.000Z",
			"source":         "cli",
		}),
		row("2026-06-03T10:00:01.000Z", "event_msg", map[string]any{
			"type":    "task_started",
			"turn_id": "turn-skill-order",
		}),
		row("2026-06-03T10:00:01.100Z", "turn_context", map[string]any{
			"model": "gpt-5.4",
		}),
		row("2026-06-03T10:00:01.200Z", "event_msg", map[string]any{
			"type":    "user_message",
			"message": "Build a dashboard",
		}),
		row("2026-06-03T10:00:02.000Z", "response_item", map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "I will read the dashboard skill instructions first."},
			},
		}),
		row("2026-06-03T10:00:02.050Z", "response_item", map[string]any{
			"type":    "function_call",
			"name":    "exec_command",
			"call_id": "call-skill-order",
			"arguments": mustJSON(map[string]any{
				"command": []string{"sed", "-n", "1,80p", userSkillFile},
			}),
		}),
		row("2026-06-03T10:00:02.200Z", "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"last_token_usage": map[string]any{
					"input_tokens":  20,
					"output_tokens": 6,
					"total_tokens":  26,
				},
			},
		}),
		row("2026-06-03T10:00:02.500Z", "event_msg", map[string]any{
			"type":    "exec_command_end",
			"call_id": "call-skill-order",
			"status":  "completed",
			"stdout":  stringsRepeat("x", 5000),
		}),
		row("2026-06-03T10:00:03.000Z", "response_item", map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "I finished reading the skill instructions."},
			},
		}),
		row("2026-06-03T10:00:03.100Z", "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"last_token_usage": map[string]any{
					"input_tokens":  30,
					"output_tokens": 10,
					"total_tokens":  40,
				},
			},
		}),
		row("2026-06-03T10:00:03.200Z", "event_msg", map[string]any{
			"type":    "agent_message",
			"message": "I finished reading the skill instructions.",
		}),
		row("2026-06-03T10:00:03.300Z", "event_msg", map[string]any{
			"type": "task_complete",
		}),
	))
	if err := os.WriteFile(rollout, body, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CollectRollout(rollout, config.Config{
		MaxChars: 4096,
		ResourceAttributes: map[string]any{
			"host":   "test-host",
			"app_id": "codex-monitor",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	root := findSpan(result.Spans, "invoke_agent", nil)
	firstLLM := findSpan(result.Spans, "llm", map[string]any{"step_index": 0})
	tool := findSpan(result.Spans, "tool:exec_command", nil)
	skill := findSpan(result.Spans, "skill:dashboard", nil)
	assistants := findSpans(result.Spans, "assistant")
	if root == nil || firstLLM == nil || tool == nil || skill == nil {
		t.Fatalf("missing expected spans: root=%v llm=%v tool=%v skill=%v", root != nil, firstLLM != nil, tool != nil, skill != nil)
	}
	if len(assistants) != 2 {
		t.Fatalf("expected 2 assistant spans, got %d", len(assistants))
	}
	for _, span := range assistants {
		if span.ParentID != root.SpanID {
			t.Fatalf("assistant parent mismatch: %#v", span)
		}
	}
	if firstLLM.ParentID != root.SpanID || tool.ParentID != root.SpanID || skill.ParentID != tool.SpanID {
		t.Fatalf("unexpected parent chain root=%s llm=%s tool=%s skill=%s", root.SpanID, firstLLM.ParentID, tool.ParentID, skill.ParentID)
	}
	for key := range root.Attributes {
		if strings.HasPrefix(key, "gen_ai.usage.") {
			t.Fatalf("invoke_agent must not carry usage attribute %s: %#v", key, root.Attributes)
		}
	}
	if firstLLM.Attributes[attrUsageInputTokens] != 20 || firstLLM.Attributes[attrUsageOutputTokens] != 6 {
		t.Fatalf("unexpected llm usage attrs: %#v", firstLLM.Attributes)
	}
	if root.Attributes["session_create_at"] != "2026-06-03T09:59:58Z" ||
		root.Attributes["session_updated_at"] != "2026-06-03T10:00:03.300Z" ||
		root.Attributes["session_channel"] != "cli" {
		t.Fatalf("unexpected session attrs: %#v", root.Attributes)
	}
	assertJSONEqual(t, root.Attributes[attrInputMessages], []map[string]any{
		{
			"role": "user",
			"parts": []any{
				map[string]any{"type": "text", "content": "Build a dashboard"},
			},
		},
	})
	assertJSONEqual(t, root.Attributes[attrOutputMessages], []map[string]any{
		{
			"role":          "assistant",
			"parts":         []any{map[string]any{"type": "text", "content": "I finished reading the skill instructions."}},
			"finish_reason": "stop",
		},
	})
	if firstLLM.DurationMs != 1200 || firstLLM.Attributes["ttft"] != int64(1000) {
		t.Fatalf("unexpected first llm timing: duration=%d attrs=%#v", firstLLM.DurationMs, firstLLM.Attributes)
	}
	if tool.Attributes[attrTriggeredByLlmSpanID] != firstLLM.SpanID {
		t.Fatalf("unexpected llm trigger id: %#v", tool.Attributes)
	}
	if tool.Attributes[attrToolCallArguments] != `{"command":["sed","-n","1,80p","`+userSkillFile+`"]}` {
		t.Fatalf("unexpected tool arguments: %#v", tool.Attributes[attrToolCallArguments])
	}
	if skill.Attributes[attrSkillName] != "dashboard" || skill.Attributes[attrSkillVersion] != "1.4.0" {
		t.Fatalf("unexpected skill attrs: %#v", skill.Attributes)
	}
	if skill.Attributes[attrSkillSourceType] != "user" || skill.Attributes[attrSkillResultStatus] != "completed" {
		t.Fatalf("unexpected skill status attrs: %#v", skill.Attributes)
	}
	if tool.Attributes[attrSkillName] != "dashboard" {
		t.Fatalf("expected tool to carry skill attrs: %#v", tool.Attributes)
	}
	if got := tool.Attributes[attrToolCallResult]; got == nil || !stringsHasSuffix(toText(got), "[truncated 904 chars]") {
		t.Fatalf("unexpected truncated tool result: %#v", got)
	}
	if root.Resource["host"] != "test-host" || root.Resource["app_id"] != "codex-monitor" {
		t.Fatalf("unexpected resource attrs: %#v", root.Resource)
	}
	if root.Resource["telemetry.sdk.language"] != "go" || root.Resource["telemetry.sdk.version"] != buildinfo.Version {
		t.Fatalf("unexpected telemetry resource attrs: %#v", root.Resource)
	}
	if root.Scope.Version != buildinfo.Version {
		t.Fatalf("unexpected instrumentation scope version: %#v", root.Scope)
	}
	assertJSONEqual(t, assistants[1].Attributes[attrOutputMessages], []map[string]any{
		{
			"role":          "assistant",
			"parts":         []any{map[string]any{"type": "text", "content": "I finished reading the skill instructions."}},
			"finish_reason": "stop",
		},
	})
}

func TestCollectRolloutCaptureNoneOmitsContentAttributes(t *testing.T) {
	base := t.TempDir()
	rollout := filepath.Join(base, "rollout.jsonl")
	body := []byte(joinJSONLines(
		row("2026-07-31T08:00:00.000Z", "session_meta", map[string]any{
			"id":                "sess-private",
			"cli_version":       "0.145.0",
			"base_instructions": "secret system instructions",
		}),
		row("2026-07-31T08:00:01.000Z", "event_msg", map[string]any{
			"type":    "task_started",
			"turn_id": "turn-private",
		}),
		row("2026-07-31T08:00:01.100Z", "event_msg", map[string]any{
			"type":    "user_message",
			"message": "secret user input",
		}),
		row("2026-07-31T08:00:02.000Z", "response_item", map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "secret assistant output"},
			},
		}),
		row("2026-07-31T08:00:02.100Z", "response_item", map[string]any{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "call-private",
			"arguments": mustJSON(map[string]any{"command": "print secret command"}),
		}),
		row("2026-07-31T08:00:02.200Z", "event_msg", map[string]any{
			"type":    "exec_command_end",
			"call_id": "call-private",
			"status":  "completed",
			"stdout":  "secret tool output",
		}),
		row("2026-07-31T08:00:02.300Z", "event_msg", map[string]any{
			"type":               "task_complete",
			"last_agent_message": "secret assistant output",
		}),
	))
	if err := os.WriteFile(rollout, body, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CollectRollout(rollout, config.Config{
		MaxChars:       4096,
		CaptureContent: "none",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Spans) == 0 {
		t.Fatal("expected telemetry spans")
	}

	contentKeys := []string{
		"input_preview",
		"output_preview",
		attrInputMessages,
		attrOutputMessages,
		attrSystemInstructions,
		attrToolDefinitions,
		attrToolCallArguments,
		attrToolCallResult,
		"tool_command",
		attrSkillPathCompat,
		attrSkillPath,
		attrSkillDescriptionCompat,
		attrSkillDescription,
	}
	secrets := []string{
		"secret system instructions",
		"secret user input",
		"secret assistant output",
		"secret command",
		"secret tool output",
	}
	for _, span := range result.Spans {
		for _, key := range contentKeys {
			if value, ok := span.Attributes[key]; ok {
				t.Fatalf("capture none leaked %q on span %q: %#v", key, span.Name, value)
			}
		}
		encoded, err := json.Marshal(span.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("capture none leaked secret %q on span %q: %s", secret, span.Name, encoded)
			}
		}
	}
}

func TestCollectRolloutSkipsBlankAndAlreadyUploadedTurns(t *testing.T) {
	base := t.TempDir()
	rollout := filepath.Join(base, "rollout.jsonl")
	body := []byte(joinJSONLines(
		row("2026-06-03T10:00:00.000Z", "session_meta", map[string]any{
			"id":          "sess-blank",
			"cli_version": "0.140.0",
		}),
		row("2026-06-03T10:00:00.100Z", "event_msg", map[string]any{
			"type":    "task_started",
			"turn_id": "turn-blank",
		}),
		row("2026-06-03T10:00:00.200Z", "turn_context", map[string]any{
			"model": "gpt-5.4",
		}),
		row("2026-06-03T10:00:00.300Z", "event_msg", map[string]any{
			"type": "task_complete",
		}),
		row("2026-06-03T10:01:00.100Z", "event_msg", map[string]any{
			"type":    "task_started",
			"turn_id": "turn-real",
		}),
		row("2026-06-03T10:01:00.200Z", "event_msg", map[string]any{
			"type":    "user_message",
			"message": "hello",
		}),
		row("2026-06-03T10:01:00.300Z", "response_item", map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "world"},
			},
		}),
		row("2026-06-03T10:01:00.400Z", "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"last_token_usage": map[string]any{"input_tokens": 3, "output_tokens": 2},
			},
		}),
		row("2026-06-03T10:01:00.500Z", "event_msg", map[string]any{
			"type": "task_complete",
		}),
	))
	if err := os.WriteFile(rollout, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout+".gtrace", []byte("turn-real\tfingerprint-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CollectRollout(rollout, config.Config{MaxChars: 4096}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Spans) != 0 {
		t.Fatalf("expected no spans when blank turn is skipped and real turn already uploaded, got %d", len(result.Spans))
	}
}

func TestCollectRolloutStitchesSubagentRolloutsIntoSameTrace(t *testing.T) {
	base := t.TempDir()
	parentDir := filepath.Join(base, "sessions", "2026", "07")
	childDir := filepath.Join(base, "sessions", "subagents")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parentRollout := filepath.Join(parentDir, "rollout-parent.jsonl")
	childRollout := filepath.Join(childDir, "rollout-child-thread-123.jsonl")

	parentBody := []byte(joinJSONLines(
		row("2026-07-24T08:00:00.000Z", "session_meta", map[string]any{
			"id":             "sess-parent",
			"cli_version":    "0.145.0",
			"model_provider": "openai",
		}),
		row("2026-07-24T08:00:01.000Z", "event_msg", map[string]any{
			"type":    "task_started",
			"turn_id": "turn-parent",
		}),
		row("2026-07-24T08:00:01.100Z", "event_msg", map[string]any{
			"type":    "user_message",
			"message": "delegate this task",
		}),
		row("2026-07-24T08:00:02.000Z", "response_item", map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "spawning subagent"},
			},
		}),
		row("2026-07-24T08:00:02.100Z", "event_msg", map[string]any{
			"type":          "collab_agent_spawn_end",
			"new_thread_id": "thread-123",
		}),
		row("2026-07-24T08:00:02.200Z", "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"last_token_usage": map[string]any{"input_tokens": 10, "output_tokens": 4},
			},
		}),
		row("2026-07-24T08:00:02.300Z", "event_msg", map[string]any{
			"type":    "agent_message",
			"message": "parent complete",
		}),
		row("2026-07-24T08:00:02.400Z", "event_msg", map[string]any{
			"type":               "task_complete",
			"last_agent_message": "parent complete",
		}),
	))
	if err := os.WriteFile(parentRollout, parentBody, 0o644); err != nil {
		t.Fatal(err)
	}

	childBody := []byte(joinJSONLines(
		row("2026-07-24T08:00:03.000Z", "session_meta", map[string]any{
			"id":             "sess-child",
			"cli_version":    "0.145.0",
			"model_provider": "openai",
		}),
		row("2026-07-24T08:00:03.100Z", "event_msg", map[string]any{
			"type":    "task_started",
			"turn_id": "turn-child",
		}),
		row("2026-07-24T08:00:03.200Z", "event_msg", map[string]any{
			"type":    "user_message",
			"message": "child work",
		}),
		row("2026-07-24T08:00:04.000Z", "response_item", map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "child result"},
			},
		}),
		row("2026-07-24T08:00:04.100Z", "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"last_token_usage": map[string]any{"input_tokens": 8, "output_tokens": 3},
			},
		}),
		row("2026-07-24T08:00:04.200Z", "event_msg", map[string]any{
			"type":               "task_complete",
			"last_agent_message": "child result",
		}),
	))
	if err := os.WriteFile(childRollout, childBody, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := CollectRollout(parentRollout, config.Config{MaxChars: 4096}, nil)
	if err != nil {
		t.Fatal(err)
	}

	parentRoot := findSpan(result.Spans, "invoke_agent", map[string]any{"run_id": "turn-parent"})
	childRoot := findSpan(result.Spans, "invoke_agent", map[string]any{"run_id": "turn-child"})
	if parentRoot == nil || childRoot == nil {
		t.Fatalf("missing parent or child root span: parent=%v child=%v", parentRoot != nil, childRoot != nil)
	}
	if childRoot.TraceID != parentRoot.TraceID {
		t.Fatalf("expected child trace id %s to match parent %s", childRoot.TraceID, parentRoot.TraceID)
	}
	if childRoot.ParentID != parentRoot.SpanID {
		t.Fatalf("expected child parent id %s to equal parent span id %s", childRoot.ParentID, parentRoot.SpanID)
	}
	if len(result.UploadedTurnStates) != 1 || result.UploadedTurnStates[0].TurnID != "turn-parent" {
		t.Fatalf("expected only root turn upload state, got %#v", result.UploadedTurnStates)
	}
}

func row(ts, typ string, payload map[string]any) map[string]any {
	return map[string]any{
		"timestamp": ts,
		"type":      typ,
		"payload":   payload,
	}
}

func joinJSONLines(rows ...map[string]any) string {
	lines := make([]string, 0, len(rows))
	for _, entry := range rows {
		lines = append(lines, mustJSON(entry))
	}
	return strings.Join(lines, "\n") + "\n"
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func findSpan(spans []model.Span, name string, attrs map[string]any) *model.Span {
	for i := range spans {
		if spans[i].Name != name {
			continue
		}
		if attrs == nil {
			return &spans[i]
		}
		matched := true
		for key, value := range attrs {
			if spans[i].Attributes[key] != value {
				matched = false
				break
			}
		}
		if matched {
			return &spans[i]
		}
	}
	return nil
}

func findSpans(spans []model.Span, name string) []model.Span {
	out := make([]model.Span, 0)
	for _, span := range spans {
		if span.Name == name {
			out = append(out, span)
		}
	}
	return out
}

func stringsRepeat(s string, count int) string {
	return strings.Repeat(s, count)
}

func stringsHasSuffix(s, suffix string) bool {
	return strings.HasSuffix(s, suffix)
}

func toText(value any) string {
	return util.ToText(value)
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("JSON mismatch\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
}
