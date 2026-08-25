package collector

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/buildinfo"
	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/config"
	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/parse"
	"github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/sidecar"
	previewcore "github.com/GuanceCloud/obs-agent-connector/internal/core/preview"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/util"
)

const (
	attrAgentName                 = "gen_ai.agent.name"
	attrAgentVersion              = "gen_ai.agent.version"
	attrConversationID            = "gen_ai.conversation.id"
	attrInputMessages             = "gen_ai.input.messages"
	attrOperationName             = "gen_ai.operation.name"
	attrOutputMessages            = "gen_ai.output.messages"
	attrOutputType                = "gen_ai.output.type"
	attrProviderName              = "gen_ai.provider.name"
	attrRequestChoiceCount        = "gen_ai.request.choice.count"
	attrRequestFrequencyPenalty   = "gen_ai.request.frequency_penalty"
	attrRequestMaxTokens          = "gen_ai.request.max_tokens"
	attrRequestModel              = "gen_ai.request.model"
	attrRequestPresencePenalty    = "gen_ai.request.presence_penalty"
	attrRequestSeed               = "gen_ai.request.seed"
	attrRequestStopSequences      = "gen_ai.request.stop_sequences"
	attrRequestTemperature        = "gen_ai.request.temperature"
	attrRequestTopP               = "gen_ai.request.top_p"
	attrResponseFinishReasons     = "gen_ai.response.finish_reasons"
	attrResponseModel             = "gen_ai.response.model"
	attrSkillDescriptionCompat    = "skill.description"
	attrSkillDescription          = "gen_ai.skill.description"
	attrSkillCallID               = "skill_call_id"
	attrSkillNameCompat           = "skill.name"
	attrSkillName                 = "gen_ai.skill.name"
	attrSkillPathCompat           = "skill.path"
	attrSkillPath                 = "gen_ai.skill.path"
	attrSkillResultStatusCompat   = "skill.result_status"
	attrSkillResultStatus         = "gen_ai.skill.result.status"
	attrSkillSourceTypeCompat     = "skill.source.type"
	attrSkillSourceType           = "gen_ai.skill.source.type"
	attrSkillVersion              = "gen_ai.skill.version"
	attrSystemInstructions        = "gen_ai.system_instructions"
	attrToolCallArguments         = "gen_ai.tool.call.arguments"
	attrToolCallID                = "gen_ai.tool.call.id"
	attrToolCallResult            = "gen_ai.tool.call.result"
	attrToolDefinitions           = "gen_ai.tool.definitions"
	attrToolName                  = "gen_ai.tool.name"
	attrTriggeredByLlmSpanID      = "triggered_by.llm_span_id"
	attrUsageCacheReadInputTokens = "gen_ai.usage.cache_read.input_tokens"
	attrUsageInputTokens          = "gen_ai.usage.input_tokens"
	attrUsageOutputTokens         = "gen_ai.usage.output_tokens"
	attrUsageReasoningOutput      = "gen_ai.usage.reasoning.output_tokens"
)

var (
	skillFilePattern    = regexp.MustCompile(`/[^\s"'` + "`" + `]+/SKILL\.md\b`)
	absolutePathPattern = regexp.MustCompile(`/[^\s"'` + "`" + `]+`)
)

type CollectContext struct {
	TraceID          string
	ParentSpanID     string
	SkillMetadata    map[string]skillMetadata
	SkipUploaded     bool
	IncludeSubagents bool
	VisitedRollouts  map[string]bool
}

type UploadedTurnState struct {
	TurnID      string
	Fingerprint string
}

type Result struct {
	SessionMeta        model.SessionMeta
	Turns              []*model.Turn
	Spans              []model.Span
	UploadedTurnStates []UploadedTurnState
	TurnBatches        []TurnBatch
}

type TurnBatch struct {
	TurnID      string
	Fingerprint string
	Spans       []model.Span
}

type usageDetails struct {
	Input                    int
	Output                   int
	CacheReadInputTokens     int
	ReasoningOutputTokens    int
	HasInput                 bool
	HasOutput                bool
	HasCacheReadInputTokens  bool
	HasReasoningOutputTokens bool
}

type skillMetadata struct {
	Description string
	Version     string
}

type skillContext struct {
	SkillName       string
	SkillFile       string
	RootPath        string
	SkillSourceType string
}

func CollectRollout(rolloutFile string, cfg config.Config, ctx *CollectContext) (Result, error) {
	normalizedRollout, err := filepath.Abs(rolloutFile)
	if err == nil {
		rolloutFile = normalizedRollout
	}
	if ctx == nil {
		ctx = &CollectContext{}
	}
	if ctx.VisitedRollouts == nil {
		ctx.VisitedRollouts = map[string]bool{}
	}
	if ctx.VisitedRollouts[rolloutFile] {
		return Result{}, nil
	}
	ctx.VisitedRollouts[rolloutFile] = true

	lines, err := parse.LoadRollout(rolloutFile)
	if err != nil {
		return Result{}, err
	}
	parsed := parse.ParseSession(lines)
	result := Result{
		SessionMeta: parsed.SessionMeta,
		Turns:       parsed.Turns,
		Spans:       make([]model.Span, 0),
	}

	var uploaded map[string]string
	if ctx.ParentSpanID == "" && !ctx.SkipUploaded {
		uploaded, err = sidecar.LoadUploadedTurnStates(rolloutFile)
		if err != nil {
			return Result{}, err
		}
	}
	skillCache := map[string]skillMetadata{}
	if ctx != nil && ctx.SkillMetadata != nil {
		skillCache = ctx.SkillMetadata
	}

	if ctx.ParentSpanID != "" {
		for _, turn := range parsed.Turns {
			if !isObservableTurn(turn, cfg.MaxChars) {
				continue
			}
			if err := populateTurnSkillMetadata(turn, skillCache); err != nil {
				return Result{}, err
			}
			built := buildTurnSpans(turn, parsed.SessionMeta, cfg, rolloutFile, ctx, skillCache)
			result.Spans = append(result.Spans, built.Spans...)
			if ctx.IncludeSubagents {
				if err := appendSubagentSpans(&result, rolloutFile, turn, parsed.SessionMeta, cfg, built, ctx, skillCache); err != nil {
					return Result{}, err
				}
			}
		}
		return result, nil
	}

	for _, turn := range parsed.Turns {
		if !isObservableTurn(turn, cfg.MaxChars) || !isTerminalTurn(turn) {
			continue
		}
		if turn.TurnID != "" && uploaded != nil {
			if _, ok := uploaded[turn.TurnID]; ok {
				continue
			}
		}
		if err := populateTurnSkillMetadata(turn, skillCache); err != nil {
			return Result{}, err
		}

		spanStart := len(result.Spans)
		built := buildTurnSpans(turn, parsed.SessionMeta, cfg, rolloutFile, ctx, skillCache)
		result.Spans = append(result.Spans, built.Spans...)
		if ctx.IncludeSubagents || ctx.ParentSpanID == "" {
			if err := appendSubagentSpans(&result, rolloutFile, turn, parsed.SessionMeta, cfg, built, ctx, skillCache); err != nil {
				return Result{}, err
			}
		}
		if turn.TurnID != "" && (ctx == nil || ctx.ParentSpanID == "") {
			fingerprint := turnFingerprint(turn)
			result.UploadedTurnStates = append(result.UploadedTurnStates, UploadedTurnState{
				TurnID:      turn.TurnID,
				Fingerprint: fingerprint,
			})
			result.TurnBatches = append(result.TurnBatches, TurnBatch{
				TurnID:      turn.TurnID,
				Fingerprint: fingerprint,
				Spans:       append([]model.Span(nil), result.Spans[spanStart:]...),
			})
		}
	}

	return result, nil
}

type buildResult struct {
	Spans      []model.Span
	TraceID    string
	RootSpanID string
}

func buildTurnSpans(turn *model.Turn, sessionMeta model.SessionMeta, cfg config.Config, rolloutFile string, ctx *CollectContext, skillCache map[string]skillMetadata) buildResult {
	maxChars := cfg.MaxChars
	if cfg.CaptureContent == "none" {
		maxChars = -1
	}
	traceID := randomTraceID()
	parentID := ""
	if ctx != nil && ctx.TraceID != "" {
		traceID = ctx.TraceID
	}
	if ctx != nil {
		parentID = ctx.ParentSpanID
	}
	rootSpanID := randomSpanID()
	if parentID != "" {
		rootSpanID = parentID
	}
	resource := resourceAttributes(cfg, sessionMeta)
	scope := model.Scope{Name: "gtrace-codex-collector", Version: buildinfo.Version, Attributes: map[string]any{}}
	ingest := map[string]any{
		"source":       "gtrace-codex-hook-go",
		"rollout_file": rolloutFile,
		"received_at":  time.Now().UTC().Format(time.RFC3339Nano),
	}

	rootAttrs := commonAttributes(cfg, sessionMeta)
	setAttr(rootAttrs, "run_id", turn.TurnID)
	setAttr(rootAttrs, "run_ids", turn.TurnID)
	setAttr(rootAttrs, attrOperationName, "invoke_agent")
	setAttr(rootAttrs, attrProviderName, sessionMeta.ModelProvider)
	setModelAttrs(rootAttrs, turn.Model)
	setRequestAttributes(rootAttrs, turn.InvocationParams, maxChars)
	setAttr(rootAttrs, attrSystemInstructions, buildSystemInstructions(sessionMeta, turn.InvocationParams, maxChars))
	setAttr(rootAttrs, attrResponseFinishReasons, []string{boolFinishReason(turn)})
	setAttr(rootAttrs, "input_preview", preview(turn.UserInput, maxChars))
	setAttr(rootAttrs, "input_length", strLen(turn.UserInput))
	setAttr(rootAttrs, "output_preview", preview(turn.FinalOutput, maxChars))
	setAttr(rootAttrs, "output_length", strLen(turn.FinalOutput))
	setAttr(rootAttrs, attrInputMessages, buildInputMessages(turn.UserInput, nil, maxChars))
	setAttr(rootAttrs, attrOutputMessages, buildOutputMessages(turn.FinalOutput, "", nil, boolFinishReason(turn), maxChars))
	setAttr(rootAttrs, "tool_count", countToolCalls(turn))
	setAttr(rootAttrs, "final_status", statusFromTurn(turn))
	setAttr(rootAttrs, "status", turnStatusValue(turn.Aborted))
	setAttr(rootAttrs, "reason", abortReason(turn.Aborted))
	setAttr(rootAttrs, "error.type", abortErrorType(turn.Aborted))
	setAttr(rootAttrs, "session_create_at", sessionMeta.CreatedAt)
	setAttr(rootAttrs, "session_updated_at", isoFromMs(turn.EndTime))
	setAttr(rootAttrs, "session_channel", sessionMeta.Channel)
	removeUsageAttrs(rootAttrs)

	spans := []model.Span{
		makeSpan(traceID, rootSpanID, parentID, "invoke_agent", turn.StartTime, turn.EndTime, rootAttrs, resource, scope, ingest, spanStatus(turn.Aborted, "")),
	}

	var previousToolResults any
	var previousToolCalls []*model.ToolCall
	for index, step := range turn.Steps {
		generationSpanID := randomSpanID()
		toolToSkill := buildSkillContexts(step.ToolCalls)
		usage := usageDetailsFromMap(step.Usage)
		llmRequestStart := llmRequestStartTime(turn, index)
		ttft := int64(0)
		if llmRequestStart <= step.StartTime {
			ttft = step.StartTime - llmRequestStart
		}
		generationInput := previousToolResults
		if index == 0 {
			generationInput = turn.UserInput
		}

		llmAttrs := commonAttributes(cfg, sessionMeta)
		setAttr(llmAttrs, "run_id", turn.TurnID)
		setAttr(llmAttrs, "run_ids", turn.TurnID)
		setAttr(llmAttrs, attrOperationName, "chat")
		setAttr(llmAttrs, attrProviderName, sessionMeta.ModelProvider)
		setModelAttrs(llmAttrs, turn.Model)
		setRequestAttributes(llmAttrs, turn.InvocationParams, maxChars)
		setAttr(llmAttrs, attrSystemInstructions, buildSystemInstructions(sessionMeta, turn.InvocationParams, maxChars))
		setAttr(llmAttrs, attrResponseFinishReasons, []string{stepFinishReason(step)})
		setAttr(llmAttrs, "input_preview", preview(generationInput, maxChars))
		setAttr(llmAttrs, "input_length", previewLen(generationInput, maxChars))
		setAttr(llmAttrs, "output_preview", preview(buildGenerationOutput(step, maxChars), maxChars))
		setAttr(llmAttrs, "output_length", previewLen(buildGenerationOutput(step, maxChars), maxChars))
		setAttr(llmAttrs, attrInputMessages, buildInputMessages(firstStepInput(index, turn.UserInput), previousToolCalls, maxChars))
		setAttr(llmAttrs, attrOutputMessages, buildOutputMessages(step.Text, step.Reasoning, step.ToolCalls, stepFinishReason(step), maxChars))
		setAttr(llmAttrs, "output_kind", outputKind(step))
		setUsageAttrs(llmAttrs, usage)
		setAttr(llmAttrs, "step_index", index)
		setAttr(llmAttrs, "ttft", ttft)
		setAttr(llmAttrs, "status", "ok")

		llmStart := step.StartTime
		if ttft > 0 {
			llmStart = llmRequestStart
		}
		llmEnd := step.EndTime
		if step.HasModelEndTime {
			llmEnd = step.ModelEndTime
		}
		spans = append(spans, makeSpan(traceID, generationSpanID, rootSpanID, "llm", llmStart, llmEnd, llmAttrs, resource, scope, ingest, model.SpanStatus{Code: "STATUS_CODE_UNSET"}))

		for messageIndex, message := range assistantMessagesFromStep(step) {
			assistantAttrs := commonAttributes(cfg, sessionMeta)
			setAttr(assistantAttrs, "run_id", turn.TurnID)
			setAttr(assistantAttrs, "run_ids", turn.TurnID)
			setAttr(assistantAttrs, attrProviderName, sessionMeta.ModelProvider)
			setModelAttrs(assistantAttrs, turn.Model)
			setAttr(assistantAttrs, attrOutputType, "text")
			setAttr(assistantAttrs, "role", "assistant")
			setAttr(assistantAttrs, "output_preview", preview(message.Text, maxChars))
			setAttr(assistantAttrs, "output_length", strLen(message.Text))
			setAttr(assistantAttrs, attrOutputMessages, buildOutputMessages(message.Text, "", nil, "stop", maxChars))
			setAttr(assistantAttrs, "output_kind", "text")
			setAttr(assistantAttrs, "assistant_message_start_time", isoFromMs(message.StartTime))
			setAttr(assistantAttrs, "assistant_message_end_time", isoFromMs(message.EndTime))
			if message.HasEventTime {
				setAttr(assistantAttrs, "assistant_message_event_time", isoFromMs(message.EventTime))
			}
			setAttr(assistantAttrs, "step_index", index)
			setAttr(assistantAttrs, "message_index", messageIndex)
			setAttr(assistantAttrs, "status", "ok")
			spans = append(spans, makeSpan(traceID, randomSpanID(), rootSpanID, "assistant", message.StartTime, message.EndTime, assistantAttrs, resource, scope, ingest, model.SpanStatus{Code: "STATUS_CODE_UNSET"}))
		}

		for _, tc := range step.ToolCalls {
			skill := toolToSkill[tc]
			meta := skillMetadata{}
			if skill != nil {
				meta = skillCache[skill.SkillFile]
			}
			toolAttrs := commonAttributes(cfg, sessionMeta)
			toolSpanID := randomSpanID()
			setAttr(toolAttrs, "run_id", turn.TurnID)
			setAttr(toolAttrs, "run_ids", turn.TurnID)
			setAttr(toolAttrs, attrOperationName, "execute_tool")
			setAttr(toolAttrs, attrProviderName, sessionMeta.ModelProvider)
			setModelAttrs(toolAttrs, turn.Model)
			setAttr(toolAttrs, attrToolName, firstNonEmptyString(tc.Name, "tool"))
			setAttr(toolAttrs, attrToolCallID, tc.CallID)
			setAttr(toolAttrs, attrTriggeredByLlmSpanID, generationSpanID)
			setAttr(toolAttrs, "tool_command", preview(toolCommand(tc), maxChars))
			setAttr(toolAttrs, attrToolCallArguments, preview(tc.Args, maxChars))
			setAttr(toolAttrs, attrToolCallResult, capturedValue(tc.Output, maxChars))
			setSkillAttributes(toolAttrs, skill, meta, skillResultStatus(tc), tc.CallID, maxChars)
			setAttr(toolAttrs, "tool_result_status", skillResultStatus(tc))
			setAttr(toolAttrs, "status", toolStatusValue(tc))
			setAttr(toolAttrs, "reason", preview(tc.Error, maxChars))
			setAttr(toolAttrs, "error.type", toolErrorType(tc))
			toolEnd := step.EndTime
			if tc.HasEnd {
				toolEnd = tc.EndTime
			}
			spans = append(spans, makeSpan(traceID, toolSpanID, rootSpanID, "tool:"+firstNonEmptyString(tc.Name, "tool"), tc.StartTime, toolEnd, toolAttrs, resource, scope, ingest, spanStatus(tc.Error != "", tc.Error)))

			if skill == nil {
				continue
			}
			skillAttrs := commonAttributes(cfg, sessionMeta)
			setAttr(skillAttrs, "run_id", turn.TurnID)
			setAttr(skillAttrs, "run_ids", turn.TurnID)
			setAttr(skillAttrs, attrOperationName, "skill")
			setAttr(skillAttrs, attrProviderName, sessionMeta.ModelProvider)
			setModelAttrs(skillAttrs, turn.Model)
			setSkillAttributes(skillAttrs, skill, meta, skillResultStatus(tc), tc.CallID, maxChars)
			setAttr(skillAttrs, "tool_count", 1)
			setAttr(skillAttrs, "input_preview", preview(skill.SkillFile, maxChars))
			setAttr(skillAttrs, "output_preview", preview(util.ToText(tc.Output), maxChars))
			setAttr(skillAttrs, "status", toolStatusValue(tc))
			setAttr(skillAttrs, "reason", preview(tc.Error, maxChars))
			setAttr(skillAttrs, "error.type", toolErrorType(tc))
			spans = append(spans, makeSpan(traceID, randomSpanID(), toolSpanID, "skill:"+skill.SkillName, tc.StartTime, toolEnd, skillAttrs, resource, scope, ingest, spanStatus(tc.Error != "", tc.Error)))
		}

		previousToolResults = previousToolOutputs(step.ToolCalls, maxChars)
		if len(step.ToolCalls) > 0 {
			previousToolCalls = step.ToolCalls
		} else {
			previousToolCalls = nil
		}
	}

	return buildResult{Spans: spans, TraceID: traceID, RootSpanID: rootSpanID}
}

func appendSubagentSpans(result *Result, rolloutFile string, turn *model.Turn, sessionMeta model.SessionMeta, cfg config.Config, built buildResult, ctx *CollectContext, skillCache map[string]skillMetadata) error {
	for _, threadID := range turn.SubagentThreadIDs {
		subFile, err := findSubagentRollout(rolloutFile, threadID)
		if err != nil || strings.TrimSpace(subFile) == "" {
			continue
		}
		subCtx := &CollectContext{
			TraceID:          built.TraceID,
			ParentSpanID:     built.RootSpanID,
			SkillMetadata:    skillCache,
			SkipUploaded:     true,
			IncludeSubagents: true,
			VisitedRollouts:  ctx.VisitedRollouts,
		}
		sub, err := CollectRollout(subFile, cfg, subCtx)
		if err != nil {
			return err
		}
		result.Spans = append(result.Spans, sub.Spans...)
	}
	_ = sessionMeta
	return nil
}

func findSubagentRollout(parentFile, threadID string) (string, error) {
	suffix := "-" + threadID + ".jsonl"
	root := filepath.Clean(filepath.Join(filepath.Dir(parentFile), "../../.."))
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if found != "" {
			return filepath.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return "", err
	}
	return found, nil
}

func randomTraceID() string {
	return randomHex(16)
}

func randomSpanID() string {
	return randomHex(8)
}

func randomHex(size int) string {
	buf := make([]byte, size)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func nsFromMs(ms int64) string {
	return strconv.FormatInt(ms*1_000_000, 10)
}

func isoFromMs(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func durationMs(start, end int64) int64 {
	if end < start {
		return 0
	}
	return end - start
}

func normalizeBounds(start, end int64) (int64, int64) {
	if end > start {
		return start, end
	}
	return start, start + 1
}

func makeSpan(traceID, spanID, parentID, name string, start, end int64, attributes, resource map[string]any, scope model.Scope, ingest map[string]any, status model.SpanStatus) model.Span {
	start, end = normalizeBounds(start, end)
	return model.Span{
		TraceID:           traceID,
		SpanID:            spanID,
		ParentID:          parentID,
		Name:              name,
		Kind:              "SPAN_KIND_INTERNAL",
		StartTimeUnixNano: nsFromMs(start),
		EndTimeUnixNano:   nsFromMs(end),
		StartTime:         isoFromMs(start),
		EndTime:           isoFromMs(end),
		DurationMs:        durationMs(start, end),
		Status:            status,
		Attributes:        attributes,
		Resource:          resource,
		Scope:             scope,
		Ingest:            ingest,
	}
}

func statusFromTurn(turn *model.Turn) string {
	if turn.Aborted {
		return "cancelled"
	}
	if turn.Completed {
		return "completed"
	}
	return "unset"
}

func isTerminalTurn(turn *model.Turn) bool {
	return turn != nil && (turn.Completed || turn.Aborted)
}

func isObservableTurn(turn *model.Turn, maxChars int) bool {
	if previewcore.Present(turn.UserInput, 1) || previewcore.Present(turn.FinalOutput, 1) {
		return true
	}
	for _, step := range turn.Steps {
		if previewcore.Present(step.Text, 1) || previewcore.Present(step.Reasoning, 1) || len(step.ToolCalls) > 0 || usageDetailsFromMap(step.Usage).hasAny() {
			return true
		}
	}
	return false
}

func preview(value any, maxChars int) any {
	return previewcore.Attr(value, maxChars)
}

func strLen(value string) any {
	if value == "" {
		return nil
	}
	return utf8.RuneCountInString(value)
}

func previewLen(value any, maxChars int) any {
	return previewcore.Length(value, maxChars)
}

func setAttr(attributes map[string]any, key string, value any) {
	if attributes == nil || key == "" || value == nil {
		return
	}
	if text, ok := value.(string); ok && text == "" {
		return
	}
	attributes[key] = value
}

func commonAttributes(cfg config.Config, sessionMeta model.SessionMeta) map[string]any {
	attributes := map[string]any{
		attrAgentName:         "codex",
		attrAgentVersion:      sessionMeta.CLIVersion,
		attrConversationID:    sessionMeta.SessionID,
		"session_id":          sessionMeta.SessionID,
		"request_type":        "user_request",
		"is_internal_request": false,
	}
	setAttr(attributes, "user_id", cfg.UserID)
	for key, value := range cfg.Metadata {
		setAttr(attributes, key, value)
	}
	return attributes
}

func resourceAttributes(cfg config.Config, sessionMeta model.SessionMeta) map[string]any {
	host, _ := os.Hostname()
	resource := map[string]any{
		"service.name":           "gtrace-codex",
		"telemetry.sdk.language": "go",
		"telemetry.sdk.name":     "gtrace",
		"telemetry.sdk.version":  buildinfo.Version,
		"host":                   host,
		"agent_runtime":          "codex",
		attrAgentVersion:         sessionMeta.CLIVersion,
		"runtime_environment":    cfg.Environment,
	}
	for key, value := range cfg.ResourceAttributes {
		setAttr(resource, key, value)
	}
	return resource
}

func setModelAttrs(attributes map[string]any, modelName string) {
	setAttr(attributes, attrRequestModel, modelName)
	setAttr(attributes, attrResponseModel, modelName)
}

func normalizeOutputType(value any) string {
	switch current := value.(type) {
	case string:
		text := strings.ToLower(strings.TrimSpace(current))
		switch {
		case text == "":
			return "text"
		case text == "text":
			return "text"
		case text == "image":
			return "image"
		case text == "speech" || text == "audio":
			return "speech"
		case strings.Contains(text, "json"):
			return "json"
		default:
			return text
		}
	case map[string]any:
		return normalizeOutputType(current["type"])
	default:
		return "text"
	}
}

func requestChoiceCount(value any) any {
	count, ok := asInt(value)
	if !ok || count == 1 {
		return nil
	}
	return count
}

func asFloat(value any) (float64, bool) {
	switch current := value.(type) {
	case float64:
		return current, true
	case float32:
		return float64(current), true
	case int:
		return float64(current), true
	default:
		return 0, false
	}
}

func stringArrayAttr(value any) any {
	switch current := value.(type) {
	case []any:
		out := make([]string, 0, len(current))
		for _, item := range current {
			text := util.ToText(item)
			if strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		if len(out) > 0 {
			return out
		}
	case string:
		if strings.TrimSpace(current) != "" {
			return []string{current}
		}
	}
	return nil
}

func normalizeToolDefinition(definition any) any {
	entry, ok := definition.(map[string]any)
	if !ok || entry == nil {
		return definition
	}
	if _, hasType := entry["type"]; hasType || entry["name"] == nil {
		return entry
	}
	normalized := map[string]any{
		"type": "function",
		"name": entry["name"],
	}
	if entry["description"] != nil {
		normalized["description"] = entry["description"]
	}
	if entry["parameters"] != nil {
		normalized["parameters"] = entry["parameters"]
	}
	return normalized
}

func buildToolDefinitions(invocationParams map[string]any, maxChars int) any {
	if invocationParams == nil || maxChars < 0 {
		return nil
	}
	raw := invocationParams["tools"]
	if raw == nil {
		raw = invocationParams["tool_definitions"]
	}
	if raw == nil {
		raw = invocationParams["available_tools"]
	}
	if raw == nil {
		raw = invocationParams["functions"]
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, util.ClipValue(normalizeToolDefinition(item), maxChars))
	}
	return out
}

func buildSystemInstructions(sessionMeta model.SessionMeta, invocationParams map[string]any, maxChars int) any {
	seen := map[string]bool{}
	out := make([]map[string]any, 0, 2)
	appendText := func(value string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			return
		}
		seen[trimmed] = true
		if part := textPart(trimmed, maxChars); part != nil {
			out = append(out, part)
		}
	}
	appendText(sessionMeta.BaseInstructions)
	if settings, ok := invocationParams["collaboration_mode"].(map[string]any); ok {
		if nested, ok := settings["settings"].(map[string]any); ok {
			appendText(asString(nested["developer_instructions"]))
		}
	}
	if len(out) == 0 {
		return nil
	}
	parts := make([]any, 0, len(out))
	for _, item := range out {
		parts = append(parts, item)
	}
	return parts
}

func setRequestAttributes(attributes map[string]any, invocationParams map[string]any, maxChars int) {
	if attributes == nil {
		return
	}
	if invocationParams == nil {
		setAttr(attributes, attrOutputType, "text")
		return
	}
	setAttr(attributes, attrOutputType, normalizeOutputType(firstNonNil(
		invocationParams["output_type"],
		invocationParams["outputType"],
		invocationParams["response_format"],
	)))
	setAttr(attributes, attrRequestChoiceCount, requestChoiceCount(firstNonNil(invocationParams["n"], invocationParams["choice_count"])))
	setAttr(attributes, attrRequestSeed, intOrNil(invocationParams["seed"]))
	setAttr(attributes, attrRequestTemperature, floatOrNil(invocationParams["temperature"]))
	setAttr(attributes, attrRequestTopP, floatOrNil(invocationParams["top_p"]))
	setAttr(attributes, attrRequestMaxTokens, intOrNil(firstNonNil(invocationParams["max_tokens"], invocationParams["max_output_tokens"])))
	setAttr(attributes, attrRequestPresencePenalty, floatOrNil(invocationParams["presence_penalty"]))
	setAttr(attributes, attrRequestFrequencyPenalty, floatOrNil(invocationParams["frequency_penalty"]))
	setAttr(attributes, attrRequestStopSequences, stringArrayAttr(firstNonNil(invocationParams["stop_sequences"], invocationParams["stop"])))
	setAttr(attributes, attrToolDefinitions, buildToolDefinitions(invocationParams, maxChars))
}

func intOrNil(value any) any {
	if number, ok := asInt(value); ok {
		return number
	}
	return nil
}

func floatOrNil(value any) any {
	if number, ok := asFloat(value); ok {
		return number
	}
	return nil
}

func usageDetailsFromMap(usage map[string]any) usageDetails {
	var out usageDetails
	if usage == nil {
		return out
	}
	if value, ok := asInt(usage["input_tokens"]); ok {
		out.Input = value
		out.HasInput = true
	}
	if value, ok := asInt(usage["output_tokens"]); ok {
		out.Output = value
		out.HasOutput = true
	}
	if value, ok := asInt(usage["cached_input_tokens"]); ok {
		out.CacheReadInputTokens = value
		out.HasCacheReadInputTokens = true
	}
	if value, ok := asInt(usage["reasoning_output_tokens"]); ok {
		out.ReasoningOutputTokens = value
		out.HasReasoningOutputTokens = true
	}
	return out
}

func (u usageDetails) hasAny() bool {
	return u.HasInput || u.HasOutput || u.HasCacheReadInputTokens || u.HasReasoningOutputTokens
}

func setUsageAttrs(attributes map[string]any, usage usageDetails) {
	if usage.HasInput {
		setAttr(attributes, attrUsageInputTokens, usage.Input)
	}
	if usage.HasOutput {
		setAttr(attributes, attrUsageOutputTokens, usage.Output)
	}
	if usage.HasCacheReadInputTokens {
		setAttr(attributes, attrUsageCacheReadInputTokens, usage.CacheReadInputTokens)
	}
	if usage.HasReasoningOutputTokens {
		setAttr(attributes, attrUsageReasoningOutput, usage.ReasoningOutputTokens)
	}
}

func removeUsageAttrs(attributes map[string]any) {
	for key := range attributes {
		if strings.HasPrefix(key, "gen_ai.usage.") {
			delete(attributes, key)
		}
	}
}

func countToolCalls(turn *model.Turn) int {
	count := 0
	for _, step := range turn.Steps {
		count += len(step.ToolCalls)
	}
	return count
}

func buildInputMessages(userInput string, toolCalls []*model.ToolCall, maxChars int) any {
	if maxChars < 0 {
		return nil
	}
	messages := make([]map[string]any, 0)
	if part := textPart(userInput, maxChars); part != nil {
		messages = append(messages, map[string]any{"role": "user", "parts": []any{part}})
	}
	for _, tc := range toolCalls {
		part := toolCallResponsePart(tc, maxChars)
		if part == nil {
			continue
		}
		message := map[string]any{"role": "tool", "parts": []any{part}}
		setAttr(message, "name", tc.Name)
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return nil
	}
	return messages
}

func textPart(value any, maxChars int) map[string]any {
	content := messageValue(value, maxChars)
	if content == nil {
		return nil
	}
	return map[string]any{"type": "text", "content": content}
}

func reasoningPart(value any, maxChars int) map[string]any {
	content := messageValue(value, maxChars)
	if content == nil {
		return nil
	}
	return map[string]any{"type": "reasoning", "content": content}
}

func messageValue(value any, maxChars int) any {
	if value == nil || maxChars < 0 {
		return nil
	}
	text := util.ToText(value)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return util.ClipValue(text, maxChars)
}

func toolCallRequestPart(tc *model.ToolCall, maxChars int) map[string]any {
	if tc == nil || tc.Name == "" {
		return nil
	}
	part := map[string]any{"type": "tool_call", "name": tc.Name}
	setAttr(part, "id", tc.CallID)
	setAttr(part, "arguments", messageValue(tc.Args, maxChars))
	return part
}

func toolCallResponsePart(tc *model.ToolCall, maxChars int) map[string]any {
	output := messageValue(tc.Output, maxChars)
	errText := messageValue(tc.Error, maxChars)
	if output == nil && errText == nil {
		return nil
	}
	part := map[string]any{"type": "tool_call_response"}
	if errText == nil {
		part["response"] = output
	} else {
		response := map[string]any{}
		if output != nil {
			response["output"] = output
		}
		response["error"] = errText
		part["response"] = response
	}
	setAttr(part, "id", tc.CallID)
	return part
}

func buildOutputMessages(text, reasoning string, toolCalls []*model.ToolCall, finishReason string, maxChars int) any {
	if maxChars < 0 {
		return nil
	}
	parts := make([]any, 0)
	if part := reasoningPart(reasoning, maxChars); part != nil {
		parts = append(parts, part)
	}
	if part := textPart(text, maxChars); part != nil {
		parts = append(parts, part)
	}
	for _, tc := range toolCalls {
		if part := toolCallRequestPart(tc, maxChars); part != nil {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []map[string]any{{
		"role":          "assistant",
		"parts":         parts,
		"finish_reason": finishReason,
	}}
}

func stepFinishReason(step *model.Step) string {
	if len(step.ToolCalls) > 0 {
		return "tool_call"
	}
	return "stop"
}

func boolFinishReason(turn *model.Turn) string {
	if turn.Aborted {
		return "cancelled"
	}
	return "stop"
}

func outputKind(step *model.Step) string {
	if len(step.ToolCalls) > 0 {
		return "tool_call"
	}
	return "text"
}

func assistantMessagesFromStep(step *model.Step) []*model.AssistantMessage {
	if len(step.AssistantMessages) > 0 {
		out := make([]*model.AssistantMessage, 0, len(step.AssistantMessages))
		for _, message := range step.AssistantMessages {
			if preview(message.Text, 1) != nil {
				out = append(out, message)
			}
		}
		return out
	}
	if preview(step.Text, 1) != nil {
		return []*model.AssistantMessage{{
			Text:      step.Text,
			StartTime: step.StartTime,
			EndTime:   step.EndTime,
		}}
	}
	return nil
}

func latestStepChildEndTime(step *model.Step) int64 {
	latest := step.EndTime
	for _, message := range assistantMessagesFromStep(step) {
		if message.EndTime > latest {
			latest = message.EndTime
		}
	}
	for _, tc := range step.ToolCalls {
		if tc.HasEnd && tc.EndTime > latest {
			latest = tc.EndTime
		}
	}
	return latest
}

func llmRequestStartTime(turn *model.Turn, index int) int64 {
	if index <= 0 {
		return turn.StartTime
	}
	previous := turn.Steps[index-1]
	return latestStepChildEndTime(previous)
}

func buildGenerationOutput(step *model.Step, maxChars int) any {
	if maxChars < 0 {
		return nil
	}
	output := map[string]any{}
	if step.Text != "" {
		output["content"] = util.ClipValue(step.Text, maxChars)
	}
	if step.Reasoning != "" {
		output["reasoning"] = util.ClipValue(step.Reasoning, maxChars)
	}
	if len(step.ToolCalls) > 0 {
		toolCalls := make([]map[string]any, 0, len(step.ToolCalls))
		for _, tc := range step.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id":        tc.CallID,
				"name":      tc.Name,
				"arguments": capturedValue(tc.Args, maxChars),
			})
		}
		output["tool_calls"] = toolCalls
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

func toolCommand(tc *model.ToolCall) string {
	args, ok := tc.Args.(map[string]any)
	if !ok {
		return ""
	}
	command := args["cmd"]
	if command == nil {
		command = args["command"]
	}
	switch current := command.(type) {
	case []any:
		parts := make([]string, 0, len(current))
		for _, part := range current {
			parts = append(parts, util.ToText(part))
		}
		return strings.Join(parts, " ")
	case string:
		return current
	default:
		return ""
	}
}

func skillSourceTypeFromPath(skillFile string) string {
	switch {
	case strings.Contains(skillFile, "/.codex/skills/.system/"):
		return "system"
	case strings.Contains(skillFile, "/.codex/skills/"):
		return "user"
	default:
		return "workspace"
	}
}

func normalizeFilePath(value string) string {
	return strings.ReplaceAll(value, `\`, `/`)
}

func skillContextFromSkillFile(skillFile string) *skillContext {
	if strings.TrimSpace(skillFile) == "" {
		return nil
	}
	normalized := normalizeFilePath(skillFile)
	rootPath := filepath.ToSlash(filepath.Dir(normalized))
	parts := strings.Split(strings.Trim(rootPath, "/"), "/")
	if len(parts) == 0 {
		return nil
	}
	skillName := parts[len(parts)-1]
	if skillName == "" {
		return nil
	}
	return &skillContext{
		SkillName:       skillName,
		SkillFile:       normalized,
		RootPath:        rootPath,
		SkillSourceType: skillSourceTypeFromPath(normalized),
	}
}

func buildSkillContexts(toolCalls []*model.ToolCall) map[*model.ToolCall]*skillContext {
	contexts := map[string]*skillContext{}
	toolToSkill := map[*model.ToolCall]*skillContext{}
	for _, toolCall := range toolCalls {
		refs := detectSkillRefs(toolCall, contexts)
		if len(refs) != 1 {
			continue
		}
		ref := refs[0]
		context := contexts[ref.RootPath]
		if context == nil {
			context = &skillContext{
				SkillName:       ref.SkillName,
				SkillFile:       ref.SkillFile,
				RootPath:        ref.RootPath,
				SkillSourceType: ref.SkillSourceType,
			}
			contexts[ref.RootPath] = context
		}
		toolToSkill[toolCall] = context
	}
	return toolToSkill
}

func detectSkillRefs(toolCall *model.ToolCall, active map[string]*skillContext) []*skillContext {
	refs := map[string]*skillContext{}
	for _, text := range collectStringValues(toolCall.Args, nil) {
		for _, skillFile := range skillFilePattern.FindAllString(text, -1) {
			if direct := skillContextFromSkillFile(skillFile); direct != nil {
				refs[direct.RootPath] = direct
			}
		}
		for _, resourcePath := range absolutePathPattern.FindAllString(text, -1) {
			normalized := normalizeFilePath(resourcePath)
			for _, current := range active {
				if normalized == current.SkillFile || strings.HasPrefix(normalized, current.RootPath+"/") {
					refs[current.RootPath] = current
				}
			}
		}
	}
	out := make([]*skillContext, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref)
	}
	return out
}

func collectStringValues(value any, out []string) []string {
	switch current := value.(type) {
	case string:
		out = append(out, current)
	case []any:
		for _, entry := range current {
			out = collectStringValues(entry, out)
		}
	case map[string]any:
		for _, entry := range current {
			out = collectStringValues(entry, out)
		}
	}
	return out
}

func populateTurnSkillMetadata(turn *model.Turn, cache map[string]skillMetadata) error {
	for _, step := range turn.Steps {
		toolToSkill := buildSkillContexts(step.ToolCalls)
		for _, skill := range toolToSkill {
			if skill == nil || skill.SkillFile == "" {
				continue
			}
			if _, ok := cache[skill.SkillFile]; ok {
				continue
			}
			meta, err := loadSkillMetadata(skill.SkillFile)
			if err != nil {
				return err
			}
			cache[skill.SkillFile] = meta
		}
	}
	return nil
}

func loadSkillMetadata(skillFile string) (skillMetadata, error) {
	content, err := os.ReadFile(skillFile)
	if err != nil {
		return skillMetadata{}, nil
	}
	text := string(content)
	frontmatter := parseSkillFrontmatter(text)
	version := frontmatter["version"]
	if version == "" {
		version = frontmatter["metadata.version"]
		if version == "" {
			pkgFile := filepath.Join(filepath.Dir(skillFile), "package.json")
			if data, err := os.ReadFile(pkgFile); err == nil {
				var pkg map[string]any
				if json.Unmarshal(data, &pkg) == nil {
					version = asString(pkg["version"])
				}
			}
		}
	}
	description := frontmatter["description"]
	if description == "" {
		description = extractSkillDescription(text)
	}
	return skillMetadata{Description: description, Version: version}, nil
}

func parseSkillFrontmatter(content string) map[string]string {
	match := regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---\s*(?:\n|$)`).FindStringSubmatch(content)
	if len(match) != 2 {
		return map[string]string{}
	}
	out := map[string]string{}
	section := ""
	for _, raw := range strings.Split(match[1], "\n") {
		entry := regexp.MustCompile(`^(\s*)([A-Za-z0-9_.-]+):\s*(.*)$`).FindStringSubmatch(raw)
		if len(entry) != 4 {
			continue
		}
		indent := len(entry[1])
		key := entry[2]
		value := yamlScalar(entry[3])
		if indent == 0 {
			if value == "" {
				section = key
			} else {
				section = ""
				out[key] = value
			}
			continue
		}
		if section == "metadata" && value != "" {
			out["metadata."+key] = value
		}
	}
	return out
}

func yamlScalar(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	if (strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`)) || (strings.HasPrefix(text, `'`) && strings.HasSuffix(text, `'`)) {
		return text[1 : len(text)-1]
	}
	return text
}

func extractSkillDescription(content string) string {
	body := regexp.MustCompile(`(?s)^---\s*\n.*?\n---\s*(?:\n|$)`).ReplaceAllString(content, "")
	lines := strings.Split(body, "\n")
	inFence := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

func setSkillAttributes(attributes map[string]any, skill *skillContext, metadata skillMetadata, resultStatus, skillCallID string, maxChars int) {
	description := preview(metadata.Description, maxChars)
	if skill != nil {
		setAttr(attributes, attrSkillNameCompat, skill.SkillName)
		setAttr(attributes, attrSkillSourceTypeCompat, skill.SkillSourceType)
		setAttr(attributes, attrSkillCallID, skillCallID)
		setAttr(attributes, attrSkillName, skill.SkillName)
		setAttr(attributes, attrSkillSourceType, skill.SkillSourceType)
		if maxChars >= 0 {
			setAttr(attributes, attrSkillPathCompat, skill.SkillFile)
			setAttr(attributes, attrSkillPath, skill.SkillFile)
		}
	}
	setAttr(attributes, attrSkillDescriptionCompat, description)
	setAttr(attributes, attrSkillResultStatusCompat, resultStatus)
	setAttr(attributes, attrSkillDescription, description)
	setAttr(attributes, attrSkillResultStatus, resultStatus)
	setAttr(attributes, attrSkillVersion, metadata.Version)
}

func skillResultStatus(tc *model.ToolCall) string {
	if tc != nil && tc.Error != "" {
		return "error"
	}
	return "completed"
}

func toolStatusValue(tc *model.ToolCall) string {
	if tc != nil && tc.Error != "" {
		return "error"
	}
	return "ok"
}

func toolErrorType(tc *model.ToolCall) any {
	if tc != nil && tc.Error != "" {
		return "_OTHER"
	}
	return nil
}

func turnStatusValue(aborted bool) string {
	if aborted {
		return "error"
	}
	return "ok"
}

func abortReason(aborted bool) any {
	if aborted {
		return "Turn interrupted by user"
	}
	return nil
}

func abortErrorType(aborted bool) any {
	if aborted {
		return "_OTHER"
	}
	return nil
}

func spanStatus(hasError bool, message string) model.SpanStatus {
	if hasError {
		return model.SpanStatus{Code: "STATUS_CODE_ERROR", Message: message}
	}
	return model.SpanStatus{Code: "STATUS_CODE_UNSET"}
}

func previousToolOutputs(toolCalls []*model.ToolCall, maxChars int) any {
	if len(toolCalls) == 0 || maxChars < 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(toolCalls))
	for _, tc := range toolCalls {
		entry := map[string]any{"name": tc.Name}
		if tc.Output != nil {
			entry["output"] = util.ClipValue(util.ToText(tc.Output), maxChars)
		}
		if tc.Error != "" {
			entry["error"] = util.ClipValue(tc.Error, maxChars)
		}
		out = append(out, entry)
	}
	return out
}

func capturedValue(value any, maxChars int) any {
	if maxChars < 0 {
		return nil
	}
	return util.ClipValue(value, maxChars)
}

func firstStepInput(index int, userInput string) string {
	if index == 0 {
		return userInput
	}
	return ""
}

func turnFingerprint(turn *model.Turn) string {
	payload := map[string]any{
		"turnId":            turn.TurnID,
		"completed":         turn.Completed,
		"aborted":           turn.Aborted,
		"startTime":         turn.StartTime,
		"endTime":           turn.EndTime,
		"model":             turn.Model,
		"userInput":         turn.UserInput,
		"finalOutput":       turn.FinalOutput,
		"subagentThreadIds": turn.SubagentThreadIDs,
		"steps":             turn.Steps,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func asInt(value any) (int, bool) {
	switch current := value.(type) {
	case float64:
		return int(current), true
	case int:
		return current, true
	default:
		return 0, false
	}
}

func asString(value any) string {
	switch current := value.(type) {
	case string:
		return current
	default:
		return ""
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
