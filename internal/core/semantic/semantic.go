package semantic

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
)

type Builder struct {
	ScopeName    string
	ScopeVersion string
}

var fallbackIDCounter atomic.Uint64

func (b Builder) Build(turn model.Turn) []model.Span {
	if !observable(turn) || !terminal(turn.FinalStatus) {
		return nil
	}
	start, end := normalizeWindow(turn.StartUnixNano, turn.EndUnixNano, 0, 0)
	if start == 0 || end == 0 {
		return nil
	}

	traceID := randomHex(16)
	rootID := randomHex(8)
	scope := model.Scope{
		Name:       firstNonEmpty(b.ScopeName, "gtrace-agent-core"),
		Version:    b.ScopeVersion,
		Attributes: map[string]any{},
	}
	resource := copyMap(turn.Resource)
	setDefault(resource, "service.name", "gtrace-"+firstNonEmpty(turn.AgentRuntime, "agent"))
	setDefault(resource, "telemetry.sdk.language", "go")
	setDefault(resource, "telemetry.sdk.name", "gtrace")
	setDefault(resource, "telemetry.sdk.version", b.ScopeVersion)
	setDefault(resource, "agent_runtime", turn.AgentRuntime)
	setDefault(resource, "agent_name", turn.AgentName)
	setDefault(resource, "agent_version", turn.AgentVersion)

	rootAttrs := commonAttrs(turn.SessionID, turn.AgentName, turn.AgentVersion)
	rootAttrs["gen_ai.operation.name"] = "invoke_agent"
	rootAttrs["final_status"] = string(turn.FinalStatus)
	rootAttrs["status"] = statusValue(turn.ErrorType)
	setAttr(rootAttrs, "error.type", turn.ErrorType)
	setAttr(rootAttrs, "reason", turn.Reason)
	setAttr(rootAttrs, "gen_ai.input.messages", turn.InputMessages)
	setAttr(rootAttrs, "gen_ai.output.messages", turn.OutputMessages)
	setAttr(rootAttrs, "input_preview", turn.InputPreview)
	setAttr(rootAttrs, "output_preview", turn.OutputPreview)
	setAttr(rootAttrs, "input_length", positiveInt(turn.InputLength))
	setAttr(rootAttrs, "output_length", positiveInt(turn.OutputLength))
	setAttr(rootAttrs, "tool_count", len(turn.ToolCalls))
	mergeAttrs(rootAttrs, turn.ExtraAttributes)
	removeUsageAttrs(rootAttrs)
	setAttr(rootAttrs, "gen_ai.usage.credit", positiveFloat(turn.CreditUsage))

	spans := []model.Span{makeSpan(
		traceID, rootID, "", "invoke_agent", start, end,
		rootAttrs, resource, scope, turn.ErrorType,
	)}
	llmSpanByCallID := map[string]string{}

	for _, call := range turn.LLMCalls {
		callStart, callEnd := normalizeWindow(call.StartUnixNano, call.EndUnixNano, start, end)
		spanID := randomHex(8)
		attrs := commonAttrs(turn.SessionID, turn.AgentName, turn.AgentVersion)
		attrs["gen_ai.operation.name"] = "chat"
		attrs["status"] = firstNonEmpty(call.Status, statusValue(call.ErrorType))
		setAttr(attrs, "gen_ai.provider.name", call.Provider)
		setAttr(attrs, "gen_ai.request.model", call.RequestModel)
		setAttr(attrs, "gen_ai.response.model", call.ResponseModel)
		setAttr(attrs, "gen_ai.input.messages", call.InputMessages)
		setAttr(attrs, "gen_ai.output.messages", call.OutputMessages)
		setAttr(attrs, "gen_ai.response.finish_reasons", call.FinishReasons)
		setAttr(attrs, "input_preview", call.InputPreview)
		setAttr(attrs, "output_preview", call.OutputPreview)
		setAttr(attrs, "output_kind", call.OutputKind)
		setAttr(attrs, "ttft", positiveFloat(call.TTFTMs))
		setAttr(attrs, "error.type", call.ErrorType)
		setAttr(attrs, "reason", call.Reason)
		addUsage(attrs, call.Usage)
		mergeAttrs(attrs, call.ExtraAttributes)
		spans = append(spans, makeSpan(
			traceID, spanID, rootID, "llm", callStart, callEnd,
			attrs, resource, scope, call.ErrorType,
		))
		if call.CallID != "" {
			llmSpanByCallID[call.CallID] = spanID
		}
	}

	for _, tool := range turn.ToolCalls {
		toolStart, toolEnd := normalizeWindow(tool.StartUnixNano, tool.EndUnixNano, start, end)
		toolID := randomHex(8)
		toolName := firstNonEmpty(tool.Name, "unknown")
		attrs := commonAttrs(turn.SessionID, turn.AgentName, turn.AgentVersion)
		attrs["gen_ai.operation.name"] = "execute_tool"
		attrs["status"] = firstNonEmpty(tool.Status, statusValue(tool.ErrorType))
		setAttr(attrs, "gen_ai.tool.name", toolName)
		setAttr(attrs, "gen_ai.tool.call.id", tool.CallID)
		setAttr(attrs, "gen_ai.tool.call.arguments", tool.Arguments)
		setAttr(attrs, "gen_ai.tool.call.result", tool.Result)
		setAttr(attrs, "tool_command", tool.Command)
		setAttr(attrs, "tool_result_status", tool.ResultStatus)
		setAttr(attrs, "triggered_by.llm_span_id", llmSpanByCallID[tool.TriggeringLLMCall])
		setAttr(attrs, "input_preview", tool.InputPreview)
		setAttr(attrs, "output_preview", tool.OutputPreview)
		setAttr(attrs, "error.type", tool.ErrorType)
		setAttr(attrs, "reason", tool.Reason)
		if tool.Skill != nil {
			addSkillAttrs(attrs, *tool.Skill)
		}
		mergeAttrs(attrs, tool.ExtraAttributes)
		spans = append(spans, makeSpan(
			traceID, toolID, rootID, "tool:"+toolName, toolStart, toolEnd,
			attrs, resource, scope, tool.ErrorType,
		))

		if tool.Skill != nil && strings.TrimSpace(tool.Skill.Name) != "" {
			skill := *tool.Skill
			skillAttrs := commonAttrs(turn.SessionID, turn.AgentName, turn.AgentVersion)
			skillAttrs["gen_ai.operation.name"] = "skill"
			skillAttrs["status"] = firstNonEmpty(skill.Status, statusValue(skill.ErrorType))
			setAttr(skillAttrs, "error.type", skill.ErrorType)
			setAttr(skillAttrs, "reason", skill.Reason)
			addSkillAttrs(skillAttrs, skill)
			spans = append(spans, makeSpan(
				traceID, randomHex(8), toolID, "skill:"+skill.Name,
				toolStart, toolEnd, skillAttrs, resource, scope, skill.ErrorType,
			))
		}
	}

	for _, output := range turn.AssistantOutputs {
		outputStart, outputEnd := normalizeWindow(output.StartUnixNano, output.EndUnixNano, start, end)
		attrs := commonAttrs(turn.SessionID, turn.AgentName, turn.AgentVersion)
		attrs["status"] = firstNonEmpty(output.Status, statusValue(output.ErrorType))
		setAttr(attrs, "role", "assistant")
		setAttr(attrs, "gen_ai.output.messages", output.OutputMessages)
		setAttr(attrs, "output_preview", output.OutputPreview)
		setAttr(attrs, "output_kind", output.OutputKind)
		setAttr(attrs, "gen_ai.provider.name", output.Provider)
		setAttr(attrs, "gen_ai.request.model", output.RequestModel)
		setAttr(attrs, "gen_ai.response.model", output.ResponseModel)
		setAttr(attrs, "error.type", output.ErrorType)
		setAttr(attrs, "reason", output.Reason)
		mergeAttrs(attrs, output.ExtraAttributes)
		spans = append(spans, makeSpan(
			traceID, randomHex(8), rootID, "assistant", outputStart, outputEnd,
			attrs, resource, scope, output.ErrorType,
		))
	}
	return spans
}

func observable(turn model.Turn) bool {
	return strings.TrimSpace(turn.InputPreview) != "" ||
		strings.TrimSpace(turn.OutputPreview) != "" ||
		strings.TrimSpace(turn.ErrorType) != "" ||
		len(turn.LLMCalls) > 0 ||
		len(turn.ToolCalls) > 0
}

func terminal(status model.FinalStatus) bool {
	return status == model.FinalStatusCompleted || status == model.FinalStatusCancelled
}

func normalizeWindow(valueStart, valueEnd, parentStart, parentEnd int64) (int64, int64) {
	start := valueStart
	end := valueEnd
	if parentStart > 0 && (start <= 0 || start < parentStart) {
		start = parentStart
	}
	if parentEnd > 0 && (end <= 0 || end > parentEnd) {
		end = parentEnd
	}
	if start <= 0 && end > 0 {
		start = end - 1
	}
	if end <= 0 && start > 0 {
		end = start + 1
	}
	if end <= start {
		if parentEnd > start {
			end = start + 1
			if end > parentEnd {
				end = parentEnd
			}
		} else if parentEnd > parentStart && parentEnd > 0 {
			start = parentEnd - 1
			end = parentEnd
		} else {
			end = start + 1
		}
	}
	return start, end
}

func makeSpan(
	traceID, spanID, parentID, name string,
	start, end int64,
	attributes, resource map[string]any,
	scope model.Scope,
	errorType string,
) model.Span {
	status := model.SpanStatus{Code: "STATUS_CODE_UNSET"}
	if errorType != "" {
		status.Code = "STATUS_CODE_ERROR"
		status.Message = errorType
	}
	return model.Span{
		TraceID:           traceID,
		SpanID:            spanID,
		ParentID:          parentID,
		Name:              name,
		Kind:              "SPAN_KIND_INTERNAL",
		StartTimeUnixNano: fmt.Sprintf("%d", start),
		EndTimeUnixNano:   fmt.Sprintf("%d", end),
		StartTime:         time.Unix(0, start).UTC().Format(time.RFC3339Nano),
		EndTime:           time.Unix(0, end).UTC().Format(time.RFC3339Nano),
		DurationMs:        maxInt64(1, (end-start)/int64(time.Millisecond)),
		Status:            status,
		Attributes:        attributes,
		Resource:          resource,
		Scope:             scope,
		Ingest:            map[string]any{},
	}
}

func commonAttrs(sessionID, agentName, agentVersion string) map[string]any {
	attrs := map[string]any{
		"gen_ai.conversation.id": sessionID,
		"session_id":             sessionID,
	}
	setAttr(attrs, "gen_ai.agent.name", agentName)
	setAttr(attrs, "gen_ai.agent.version", agentVersion)
	return attrs
}

func addUsage(attrs map[string]any, usage model.Usage) {
	setAttr(attrs, "gen_ai.usage.input_tokens", positiveInt64(usage.InputTokens))
	setAttr(attrs, "gen_ai.usage.output_tokens", positiveInt64(usage.OutputTokens))
	setAttr(attrs, "gen_ai.usage.cache_read.input_tokens", positiveInt64(usage.CacheReadTokens))
	setAttr(attrs, "gen_ai.usage.cache_creation.input_tokens", positiveInt64(usage.CacheCreateTokens))
	setAttr(attrs, "gen_ai.usage.reasoning.output_tokens", positiveInt64(usage.ReasoningTokens))
}

func removeUsageAttrs(attrs map[string]any) {
	for key := range attrs {
		if strings.HasPrefix(key, "gen_ai.usage.") {
			delete(attrs, key)
		}
	}
}

func addSkillAttrs(attrs map[string]any, skill model.SkillUse) {
	setAttr(attrs, "gen_ai.skill.name", skill.Name)
	setAttr(attrs, "skill.name", skill.Name)
	setAttr(attrs, "gen_ai.skill.path", skill.Path)
	setAttr(attrs, "skill.path", skill.Path)
	setAttr(attrs, "gen_ai.skill.source.type", skill.SourceType)
	setAttr(attrs, "skill.source.type", skill.SourceType)
	setAttr(attrs, "gen_ai.skill.result.status", skill.Status)
	setAttr(attrs, "skill.result_status", skill.Status)
	setAttr(attrs, "gen_ai.skill.description", skill.Description)
	setAttr(attrs, "skill.description", skill.Description)
	setAttr(attrs, "gen_ai.skill.version", skill.Version)
	setAttr(attrs, "input_preview", skill.InputPreview)
	setAttr(attrs, "output_preview", skill.OutputPreview)
	setAttr(attrs, "skill_call_id", skill.CallID)
}

func statusValue(errorType string) string {
	if errorType != "" {
		return "error"
	}
	return "ok"
}

func mergeAttrs(target, extra map[string]any) {
	for key, value := range extra {
		setAttr(target, key, value)
	}
}

func setAttr(attrs map[string]any, key string, value any) {
	if key == "" || value == nil {
		return
	}
	if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
		return
	}
	if values, ok := value.([]string); ok && len(values) == 0 {
		return
	}
	attrs[key] = value
}

func setDefault(values map[string]any, key string, value any) {
	if values[key] == nil || values[key] == "" {
		setAttr(values, key, value)
	}
}

func copyMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func randomHex(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		fallback := sha256.Sum256([]byte(fmt.Sprintf(
			"%d:%d",
			time.Now().UnixNano(),
			fallbackIDCounter.Add(1),
		)))
		copy(value, fallback[:])
	}
	return hex.EncodeToString(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func positiveInt(value int) any {
	if value > 0 {
		return value
	}
	return nil
}

func positiveInt64(value int64) any {
	if value > 0 {
		return value
	}
	return nil
}

func positiveFloat(value float64) any {
	if value > 0 {
		return value
	}
	return nil
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
