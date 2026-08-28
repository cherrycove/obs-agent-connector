package parse

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/preview"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/privacy"
)

type JournalEvent struct {
	Event        string         `json:"event"`
	RecordedNano int64          `json:"recorded_unix_nano"`
	Payload      map[string]any `json:"payload"`
}

type Options struct {
	TranscriptPath     string
	SessionID          string
	TurnID             string
	Cwd                string
	AgentVersion       string
	CaptureContent     string
	MaxChars           int
	ResourceAttributes map[string]any
	Events             []JournalEvent
}

type transcriptRecord struct {
	Timestamp uint64 `json:"timestamp"`
	Method    string `json:"method"`
	Params    struct {
		SessionID string         `json:"sessionId"`
		Update    map[string]any `json:"update"`
	} `json:"params"`
	Index int `json:"-"`
}

type responseBoundary struct {
	Start    *transcriptRecord
	Complete transcriptRecord
}

type toolBoundary struct {
	ID               string
	Name             string
	Pre              JournalEvent
	Post             JournalEvent
	Failure          bool
	PermissionDenied bool
	SubagentID       string
	SubagentType     string
}

type terminalEvidence struct {
	Event       JournalEvent
	Kind        string
	Reason      string
	Details     string
	CancelledBy string
	Output      string
	Explicit    bool
}

func ReadTurn(options Options) (model.Turn, bool, error) {
	if strings.TrimSpace(options.SessionID) == "" {
		return model.Turn{}, false, errors.New("Grok sessionId is empty")
	}
	if strings.TrimSpace(options.TurnID) == "" {
		return model.Turn{}, false, errors.New("Grok promptId is empty")
	}
	if !hasEvent(options.Events, "UserPromptSubmit") {
		return model.Turn{}, false, nil
	}

	hookTerminal := findTerminalEvent(options.Events)
	records := make([]transcriptRecord, 0)
	if strings.TrimSpace(options.TranscriptPath) != "" {
		var err error
		records, err = readTranscript(options.TranscriptPath, options.SessionID)
		if err != nil && !hookTerminal.Explicit {
			return model.Turn{}, false, err
		}
	}
	terminalIndex, terminalRecord := findTurnCompleted(records, options.TurnID)
	if terminalRecord == nil && !hookTerminal.Explicit {
		return model.Turn{}, false, nil
	}

	prompt := latestEventString(options.Events, "UserPromptSubmit", "prompt")
	output := hookTerminal.Output
	stopReason := hookTerminal.Reason
	if terminalRecord != nil {
		stopReason = firstNonEmpty(stringValue(terminalRecord.Params.Update, "stop_reason", "stopReason"), stopReason)
		output = firstNonEmpty(stringValue(terminalRecord.Params.Update, "agent_result", "agentResult"), output)
	}
	tools := collectTools(options.Events)
	responses := collectResponses(records, terminalIndex)
	if strings.TrimSpace(prompt) == "" && strings.TrimSpace(output) == "" && len(tools) == 0 && len(responses) == 0 && hookTerminal.Kind == "" {
		return model.Turn{}, false, nil
	}

	turn := normalize(options, prompt, output, stopReason, hookTerminal, terminalRecord, responses, tools)
	if turn.FinalStatus == model.FinalStatusUnset {
		return model.Turn{}, false, nil
	}
	return turn, true, nil
}

// CompletedTurnIDs returns durable prompt IDs from valid TurnCompleted records.
// Invalid and incomplete JSONL lines are ignored so one bad tail cannot hide
// earlier completed turns.
func CompletedTurnIDs(path, sessionID string) ([]string, error) {
	records, err := readTranscript(path, sessionID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, record := range records {
		if updateType(record) != "turn_completed" {
			continue
		}
		turnID := stringValue(record.Params.Update, "prompt_id", "promptId")
		if turnID != "" && !seen[turnID] {
			seen[turnID] = true
			out = append(out, turnID)
		}
	}
	return out, nil
}

func readTranscript(path, sessionID string) ([]transcriptRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records := make([]transcriptRecord, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	index := 0
	for scanner.Scan() {
		var record transcriptRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			index++
			continue
		}
		record.Index = index
		index++
		if record.Method != "_x.ai/session/update" || len(record.Params.Update) == 0 {
			continue
		}
		if record.Params.SessionID != "" && sessionID != "" && record.Params.SessionID != sessionID {
			continue
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

func findTurnCompleted(records []transcriptRecord, turnID string) (int, *transcriptRecord) {
	for index := len(records) - 1; index >= 0; index-- {
		if updateType(records[index]) == "turn_completed" && stringValue(records[index].Params.Update, "prompt_id", "promptId") == turnID {
			return index, &records[index]
		}
	}
	return -1, nil
}

func collectResponses(records []transcriptRecord, terminalIndex int) []responseBoundary {
	if terminalIndex < 0 {
		terminalIndex = len(records)
	}
	startIndex := 0
	for index := terminalIndex - 1; index >= 0; index-- {
		if updateType(records[index]) == "turn_completed" {
			startIndex = index + 1
			break
		}
	}
	pending := make([]*transcriptRecord, 0)
	byID := map[string]*transcriptRecord{}
	out := make([]responseBoundary, 0)
	for index := startIndex; index < terminalIndex; index++ {
		record := &records[index]
		switch updateType(*record) {
		case "response_started":
			pending = append(pending, record)
			if id := stringValue(record.Params.Update, "message_id", "messageId"); id != "" {
				byID[id] = record
			}
		case "response_completed":
			id := stringValue(record.Params.Update, "message_id", "messageId")
			start := byID[id]
			if id == "" {
				var onlyIndex = -1
				for candidate := range pending {
					if pending[candidate] == nil || stringValue(pending[candidate].Params.Update, "message_id", "messageId") != "" {
						continue
					}
					if onlyIndex >= 0 {
						onlyIndex = -2
						break
					}
					onlyIndex = candidate
				}
				if onlyIndex >= 0 {
					start = pending[onlyIndex]
					pending[onlyIndex] = nil
				}
			} else if start != nil {
				delete(byID, id)
				for candidate := range pending {
					if pending[candidate] == start {
						pending[candidate] = nil
						break
					}
				}
			}
			if start == nil {
				continue
			}
			out = append(out, responseBoundary{Start: start, Complete: *record})
		}
	}
	return out
}

func collectTools(events []JournalEvent) []toolBoundary {
	byID := map[string]*toolBoundary{}
	order := make([]string, 0)
	ensure := func(id, name string, position int) *toolBoundary {
		if id == "" {
			id = derivedID("grok-tool", name, strconv.Itoa(position))
		}
		if value := byID[id]; value != nil {
			if value.Name == "" {
				value.Name = name
			}
			return value
		}
		value := &toolBoundary{ID: id, Name: name}
		byID[id] = value
		order = append(order, id)
		return value
	}
	for index, event := range events {
		payload := event.Payload
		switch strings.ToLower(strings.TrimSpace(event.Event)) {
		case "pretooluse":
			value := ensure(eventToolID(payload), eventToolName(payload), index)
			value.Pre = event
		case "posttooluse", "posttoolusefailure", "permissiondenied":
			value := ensure(eventToolID(payload), eventToolName(payload), index)
			value.Post = event
			value.Failure = strings.EqualFold(event.Event, "PostToolUseFailure")
			value.PermissionDenied = strings.EqualFold(event.Event, "PermissionDenied")
		case "subagentstart":
			id := stringValue(payload, "subagentId", "subagent_id")
			if id == "" {
				continue
			}
			value := ensure(id, "subagent", index)
			value.Pre = event
			value.SubagentID = id
			value.SubagentType = stringValue(payload, "subagentType", "subagent_type")
		case "subagentstop", "subagentend":
			if boolValue(firstNonNil(payload["stopHookActive"], payload["stop_hook_active"])) {
				continue
			}
			id := stringValue(payload, "subagentId", "subagent_id")
			if id == "" {
				continue
			}
			value := ensure(id, "subagent", index)
			value.Post = event
			value.SubagentID = id
			value.SubagentType = firstNonEmpty(value.SubagentType, stringValue(payload, "subagentType", "subagent_type"))
		}
	}
	out := make([]toolBoundary, 0, len(order))
	for _, id := range order {
		value := byID[id]
		if value.SubagentID != "" && (value.Pre.RecordedNano == 0 || value.Post.RecordedNano == 0) {
			continue
		}
		if value.Pre.RecordedNano == 0 && value.Post.RecordedNano == 0 {
			continue
		}
		out = append(out, *value)
	}
	return out
}

func normalize(
	options Options,
	prompt, output, stopReason string,
	hookTerminal terminalEvidence,
	terminalRecord *transcriptRecord,
	responses []responseBoundary,
	tools []toolBoundary,
) model.Turn {
	start := latestEventTime(options.Events, "UserPromptSubmit")
	end := hookTerminal.Event.RecordedNano
	if terminalRecord != nil {
		end = maxInt64(end, recordNano(*terminalRecord))
	}
	if end <= 0 {
		end = time.Now().UnixNano()
	}
	if start <= 0 || start >= end {
		start = end - int64(time.Millisecond)
	}

	finalStatus := model.FinalStatusCompleted
	errorType := ""
	reason := hookTerminal.Details
	terminalKind := strings.ToLower(hookTerminal.Kind)
	if terminalKind == "stopcancelled" || isCancelledReason(stopReason) {
		finalStatus = model.FinalStatusCancelled
		errorType = "cancelled"
		if reason == "" {
			reason = firstNonEmpty(hookTerminal.Reason, stopReason)
		}
	} else if terminalKind == "stopfailure" {
		errorType = "grok_" + normalizedErrorType(firstNonEmpty(hookTerminal.Reason, "unknown"))
		if reason == "" {
			reason = hookTerminal.Output
		}
	} else if strings.EqualFold(stopReason, "error") || strings.EqualFold(stopReason, "failed") {
		errorType = "grok_agent_error"
		if reason == "" {
			reason = output
		}
	}

	resource := copyMap(options.ResourceAttributes)
	resource["agent_runtime"] = "grok"
	resource["telemetry.sdk.language"] = "go"
	resource["telemetry.sdk.name"] = "gtrace"
	extra := map[string]any{
		"request_type":  "user_request",
		"timing.source": "grok_hooks_and_updates",
	}
	if stopReason != "" {
		extra["grok.stop_reason"] = privacy.Text(stopReason, 128)
	}
	if hookTerminal.CancelledBy != "" {
		extra["grok.cancelled_by"] = hookTerminal.CancelledBy
	}
	if subagentType := latestEventString(options.Events, "UserPromptSubmit", "subagentType", "subagent_type"); subagentType != "" {
		extra["request_type"] = "subagent"
		extra["gen_ai.subagent.type"] = subagentType
	}

	turn := model.Turn{
		SessionID: options.SessionID, TurnID: options.TurnID,
		AgentRuntime: "grok", AgentName: "Grok Build", AgentVersion: options.AgentVersion,
		StartUnixNano: start, EndUnixNano: end, FinalStatus: finalStatus,
		InputLength: len([]rune(prompt)), OutputLength: len([]rune(output)),
		Resource: resource, ExtraAttributes: extra, ErrorType: errorType, Reason: privacy.Text(reason, options.MaxChars),
	}
	if terminalRecord != nil {
		turn.Usage = promptUsage(terminalRecord.Params.Update["usage"])
	}
	if options.CaptureContent != "none" {
		turn.InputMessages = textMessage("user", prompt, options.MaxChars)
		turn.OutputMessages = textMessage("assistant", output, options.MaxChars)
		turn.InputPreview = preview.Text(prompt, options.MaxChars)
		turn.OutputPreview = preview.Text(output, options.MaxChars)
	}

	for index, response := range responses {
		call := responseCall(response, options.TurnID, index, start, end)
		turn.LLMCalls = append(turn.LLMCalls, call)
	}
	if len(turn.LLMCalls) == 0 && terminalRecord != nil {
		if call, ok := singleCallFromTurnUsage(*terminalRecord, options.TurnID, start, end); ok {
			turn.LLMCalls = append(turn.LLMCalls, call)
		}
	}

	for _, raw := range tools {
		toolStart, toolEnd, timingSource := toolWindow(raw, start, end)
		arguments := firstNonNil(raw.Pre.Payload["toolInput"], raw.Post.Payload["toolInput"], raw.Pre.Payload["tool_input"], raw.Post.Payload["tool_input"])
		result := firstNonNil(raw.Post.Payload["toolResult"], raw.Post.Payload["tool_result"])
		tool := model.ToolCall{
			CallID: raw.ID, Name: firstNonEmpty(raw.Name, eventToolName(raw.Pre.Payload), eventToolName(raw.Post.Payload), "unknown"),
			StartUnixNano: toolStart, EndUnixNano: toolEnd, Status: "ok", ResultStatus: "completed",
			ExtraAttributes: map[string]any{"timing.source": timingSource},
		}
		if raw.Failure {
			tool.Status = "error"
			tool.ResultStatus = "error"
			tool.ErrorType = "tool_error"
			tool.Reason = stringValue(raw.Post.Payload, "error")
			result = firstNonNil(raw.Post.Payload["error"], result)
		}
		if raw.PermissionDenied {
			tool.Status = "error"
			tool.ResultStatus = "permission_denied"
			tool.ErrorType = "permission_denied"
			tool.Reason = "Tool permission was denied"
		}
		if raw.SubagentID != "" {
			tool.ExtraAttributes["gen_ai.subagent.id"] = raw.SubagentID
			if raw.SubagentType != "" {
				tool.ExtraAttributes["gen_ai.subagent.type"] = raw.SubagentType
			}
		}
		if options.CaptureContent != "none" {
			tool.Arguments = privacy.Sanitize(arguments, options.MaxChars)
			tool.Result = privacy.Sanitize(result, options.MaxChars)
			tool.Command = commandValue(arguments, options.MaxChars)
			tool.InputPreview = preview.Text(arguments, options.MaxChars)
			tool.OutputPreview = preview.Text(result, options.MaxChars)
		}
		tool.Skill = skillFromTool(tool.Name, raw.ID, arguments, options.CaptureContent, options.MaxChars)
		turn.ToolCalls = append(turn.ToolCalls, tool)
	}

	if output != "" && terminalKind != "stopfailure" {
		assistantStart := end - 1
		if assistantStart < start {
			assistantStart = start
		}
		assistant := model.AssistantOutput{
			StartUnixNano: assistantStart, EndUnixNano: end, OutputKind: "text", Status: "ok",
			ExtraAttributes: map[string]any{"timing.source": "grok_turn_completed"},
		}
		if options.CaptureContent != "none" {
			assistant.OutputMessages = turn.OutputMessages
			assistant.OutputPreview = turn.OutputPreview
		}
		turn.AssistantOutputs = append(turn.AssistantOutputs, assistant)
	}
	return turn
}

func responseCall(value responseBoundary, turnID string, index int, parentStart, parentEnd int64) model.LLMCall {
	complete := value.Complete.Params.Update
	start := recordNano(value.Complete) - 1
	modelName := ""
	usage := responseUsage(complete["usage"])
	if value.Start != nil {
		start = recordNano(*value.Start)
		modelName = stringValue(value.Start.Params.Update, "model")
		if usage == (model.Usage{}) {
			usage = responseStartedUsage(value.Start.Params.Update)
		}
	}
	end := recordNano(value.Complete)
	start, end = childWindow(start, end, parentStart, parentEnd)
	callID := firstNonEmpty(stringValue(complete, "message_id", "messageId"), derivedID(turnID, "llm", strconv.Itoa(index)))
	return model.LLMCall{
		CallID: callID, StartUnixNano: start, EndUnixNano: end,
		RequestModel: modelName, FinishReasons: stringSlice(stringValue(complete, "stop_reason", "stopReason")),
		Usage: usage, Status: "ok", ExtraAttributes: map[string]any{"timing.source": "grok_updates"},
	}
}

func responseStartedUsage(update map[string]any) model.Usage {
	uncached := int64Value(update["input_tokens"])
	cacheRead := int64Value(update["cache_read_input_tokens"])
	cacheCreate := int64Value(update["cache_creation_input_tokens"])
	return model.Usage{
		InputTokens:     uncached + cacheRead + cacheCreate,
		CacheReadTokens: cacheRead, CacheCreateTokens: cacheCreate,
	}
}

func responseUsage(value any) model.Usage {
	usage, _ := value.(map[string]any)
	uncached := int64Value(usage["input_tokens"])
	cacheRead := int64Value(usage["cache_read_input_tokens"])
	cacheCreate := int64Value(usage["cache_creation_input_tokens"])
	return model.Usage{
		InputTokens:     uncached + cacheRead + cacheCreate,
		OutputTokens:    int64Value(usage["output_tokens"]),
		CacheReadTokens: cacheRead, CacheCreateTokens: cacheCreate,
		ReasoningTokens: int64Value(usage["reasoning_tokens"]),
	}
}

func promptUsage(value any) model.Usage {
	usage, _ := value.(map[string]any)
	return model.Usage{
		InputTokens:       int64Value(firstNonNil(usage["inputTokens"], usage["input_tokens"])),
		OutputTokens:      int64Value(firstNonNil(usage["outputTokens"], usage["output_tokens"])),
		CacheReadTokens:   int64Value(firstNonNil(usage["cachedReadTokens"], usage["cached_read_tokens"])),
		CacheCreateTokens: int64Value(firstNonNil(usage["cacheCreationTokens"], usage["cache_creation_tokens"])),
		ReasoningTokens:   int64Value(firstNonNil(usage["reasoningTokens"], usage["reasoning_tokens"])),
	}
}

func singleCallFromTurnUsage(record transcriptRecord, turnID string, parentStart, parentEnd int64) (model.LLMCall, bool) {
	usageValue, ok := record.Params.Update["usage"].(map[string]any)
	if !ok || boolValue(firstNonNil(usageValue["usageIsIncomplete"], usageValue["usage_is_incomplete"])) {
		return model.LLMCall{}, false
	}
	if int64Value(firstNonNil(usageValue["modelCalls"], usageValue["model_calls"])) != 1 {
		return model.LLMCall{}, false
	}
	durationMS := int64Value(firstNonNil(usageValue["apiDurationMs"], usageValue["api_duration_ms"]))
	if durationMS <= 0 {
		return model.LLMCall{}, false
	}
	usage := promptUsage(usageValue)
	if usage == (model.Usage{}) {
		return model.LLMCall{}, false
	}
	modelName, ok := singleUsageModel(firstNonNil(usageValue["modelUsage"], usageValue["model_usage"]))
	if !ok {
		return model.LLMCall{}, false
	}

	end := recordNano(record)
	start := end - durationMS*int64(time.Millisecond)
	start, end = childWindow(start, end, parentStart, parentEnd)
	return model.LLMCall{
		CallID:        derivedID(turnID, "llm", "turn-usage"),
		StartUnixNano: start,
		EndUnixNano:   end,
		ResponseModel: modelName,
		FinishReasons: stringSlice(stringValue(record.Params.Update, "stop_reason", "stopReason")),
		Usage:         usage,
		Status:        "ok",
		ExtraAttributes: map[string]any{
			"timing.source": "grok_turn_completed_usage",
		},
	}, true
}

func singleUsageModel(value any) (string, bool) {
	models, ok := value.(map[string]any)
	if !ok || len(models) != 1 {
		return "", false
	}
	for name, raw := range models {
		modelUsage, ok := raw.(map[string]any)
		if !ok || int64Value(firstNonNil(modelUsage["modelCalls"], modelUsage["model_calls"])) != 1 {
			return "", false
		}
		return strings.TrimSpace(name), strings.TrimSpace(name) != ""
	}
	return "", false
}

func findTerminalEvent(events []JournalEvent) terminalEvidence {
	var normal terminalEvidence
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		switch strings.ToLower(strings.TrimSpace(event.Event)) {
		case "stopfailure":
			return terminalEvidence{
				Event: event, Kind: "StopFailure", Explicit: true,
				Reason: stringValue(event.Payload, "error"), Details: stringValue(event.Payload, "errorDetails", "error_details"),
				Output: stringValue(event.Payload, "lastAssistantMessage", "last_assistant_message"),
			}
		case "stopcancelled":
			return terminalEvidence{
				Event: event, Kind: "StopCancelled", Explicit: true,
				Reason: stringValue(event.Payload, "reason"), Details: stringValue(event.Payload, "reasonDetails", "reason_details"),
				CancelledBy: stringValue(event.Payload, "cancelledBy", "cancelled_by"),
				Output:      stringValue(event.Payload, "lastAssistantMessage", "last_assistant_message"),
			}
		case "stop":
			if boolValue(firstNonNil(event.Payload["stopHookActive"], event.Payload["stop_hook_active"])) {
				continue
			}
			if normal.Kind == "" {
				normal = terminalEvidence{
					Event: event, Kind: "Stop", Reason: stringValue(event.Payload, "reason"),
					Output: stringValue(event.Payload, "lastAssistantMessage", "last_assistant_message"),
				}
			}
		}
	}
	return normal
}

func skillFromTool(toolName, callID string, input any, capture string, maxChars int) *model.SkillUse {
	name := strings.ToLower(strings.TrimSpace(toolName))
	if !(strings.Contains(name, "read") || name == "view_file" || name == "open_file") {
		return nil
	}
	path := skillPath(input, "")
	if path == "" {
		return nil
	}
	skillName := filepath.Base(filepath.Dir(filepath.Clean(path)))
	if skillName == "." || skillName == string(filepath.Separator) || skillName == "" {
		return nil
	}
	skill := &model.SkillUse{Name: skillName, CallID: callID, SourceType: "filesystem", Status: "ok"}
	if capture != "none" {
		skill.Path = privacy.Text(path, maxChars)
	}
	return skill
}

func skillPath(value any, key string) string {
	switch current := value.(type) {
	case map[string]any:
		for childKey, child := range current {
			if path := skillPath(child, childKey); path != "" {
				return path
			}
		}
	case []any:
		for _, child := range current {
			if path := skillPath(child, key); path != "" {
				return path
			}
		}
	case string:
		normalizedKey := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		if (strings.Contains(normalizedKey, "path") || strings.Contains(normalizedKey, "file")) && strings.EqualFold(filepath.Base(filepath.Clean(current)), "SKILL.md") {
			return current
		}
	}
	return ""
}

func toolWindow(value toolBoundary, parentStart, parentEnd int64) (int64, int64, string) {
	start := value.Pre.RecordedNano
	end := value.Post.RecordedNano
	duration := int64Value(firstNonNil(value.Post.Payload["durationMs"], value.Post.Payload["duration_ms"]))
	source := "grok_hooks"
	if duration > 0 && end > 0 {
		start = end - duration*int64(time.Millisecond)
		source = "grok_duration_ms"
	}
	start, end = childWindow(start, end, parentStart, parentEnd)
	return start, end, source
}

func childWindow(start, end, parentStart, parentEnd int64) (int64, int64) {
	if start <= 0 || start < parentStart {
		start = parentStart
	}
	if end <= 0 {
		end = start + 1
	}
	if end > parentEnd {
		end = parentEnd
	}
	if end <= start {
		if start < parentEnd {
			end = start + 1
		} else {
			start = parentEnd - 1
			end = parentEnd
		}
	}
	return start, end
}

func recordNano(record transcriptRecord) int64 {
	if record.Timestamp == 0 {
		return 0
	}
	return int64(record.Timestamp)*int64(time.Second) + int64(record.Index)
}

func updateType(record transcriptRecord) string {
	return strings.ToLower(strings.TrimSpace(stringValue(record.Params.Update, "sessionUpdate", "session_update")))
}

func latestEventString(events []JournalEvent, eventName string, keys ...string) string {
	for index := len(events) - 1; index >= 0; index-- {
		if strings.EqualFold(events[index].Event, eventName) {
			return stringValue(events[index].Payload, keys...)
		}
	}
	return ""
}

func latestEventTime(events []JournalEvent, eventName string) int64 {
	for index := len(events) - 1; index >= 0; index-- {
		if strings.EqualFold(events[index].Event, eventName) {
			return events[index].RecordedNano
		}
	}
	return 0
}

func hasEvent(events []JournalEvent, name string) bool {
	for _, event := range events {
		if strings.EqualFold(event.Event, name) {
			return true
		}
	}
	return false
}

func isCancelledReason(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "cancelled", "canceled", "user_interrupt", "permission_rejected", "permission_cancelled", "max_turns", "no_progress":
		return true
	}
	return false
}

func normalizedErrorType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	var out strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' {
			out.WriteRune(char)
		} else {
			out.WriteByte('_')
		}
	}
	return strings.Trim(out.String(), "_")
}

func textMessage(role, text string, maxChars int) any {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []any{map[string]any{"role": role, "parts": []any{map[string]any{"type": "text", "content": privacy.Text(text, maxChars)}}}}
}

func commandValue(value any, maxChars int) string {
	current, _ := value.(map[string]any)
	for _, key := range []string{"command", "cmd", "script"} {
		if text := stringValue(current, key); text != "" {
			return privacy.Text(text, maxChars)
		}
	}
	return ""
}

func eventToolID(value map[string]any) string {
	return stringValue(value, "toolUseId", "tool_use_id", "toolCallId", "tool_call_id", "id")
}

func eventToolName(value map[string]any) string {
	return stringValue(value, "toolName", "tool_name", "name")
}

func stringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func stringSlice(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []string{strings.TrimSpace(value)}
}

func int64Value(value any) int64 {
	switch current := value.(type) {
	case float64:
		return int64(current)
	case int64:
		return current
	case int:
		return int64(current)
	case json.Number:
		parsed, _ := current.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(current, 10, 64)
		return parsed
	}
	return 0
}

func boolValue(value any) bool {
	switch current := value.(type) {
	case bool:
		return current
	case string:
		parsed, _ := strconv.ParseBool(strings.TrimSpace(current))
		return parsed
	case float64:
		return current != 0
	}
	return false
}

func copyMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
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

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func derivedID(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:12])
}
