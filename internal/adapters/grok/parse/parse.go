package parse

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
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

type sessionEventRecord struct {
	Timestamp     string `json:"ts"`
	Type          string `json:"type"`
	SessionID     string `json:"session_id"`
	SchemaVersion string `json:"schema_version"`
	ModelID       string `json:"model_id"`
	Phase         string `json:"phase"`
	Outcome       string `json:"outcome"`
	LoopIndex     int64  `json:"loop_index"`
	UnixNano      int64  `json:"-"`
}

type sessionTurnBlock struct {
	StartUnixNano int64
	EndUnixNano   int64
	SessionID     string
	ModelID       string
	Events        []sessionEventRecord
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

type toolExecutionCluster struct {
	StartUnixNano int64
	EndUnixNano   int64
	ToolIDs       []string
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

const maxSessionEventHookSkew = 2 * time.Second

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
	eventTurns := make([]sessionTurnBlock, 0)
	if len(responses) == 0 && strings.TrimSpace(options.TranscriptPath) != "" {
		eventsPath := filepath.Join(filepath.Dir(options.TranscriptPath), "events.jsonl")
		eventTurns, _ = readSessionTurnBlocks(eventsPath)
	}
	if strings.TrimSpace(prompt) == "" && strings.TrimSpace(output) == "" && len(tools) == 0 && len(responses) == 0 && hookTerminal.Kind == "" {
		return model.Turn{}, false, nil
	}

	turn := normalize(options, prompt, output, stopReason, hookTerminal, terminalRecord, responses, eventTurns, tools)
	if options.CaptureContent != "none" && terminalRecord != nil && strings.TrimSpace(options.TranscriptPath) != "" {
		enrichCallsFromChatHistory(&turn, options.TranscriptPath, *terminalRecord, options.MaxChars)
	}
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

func readSessionTurnBlocks(path string) ([]sessionTurnBlock, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	blocks := make([]sessionTurnBlock, 0)
	var active *sessionTurnBlock
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var event sessionEventRecord
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, event.Timestamp)
		if err != nil {
			continue
		}
		event.UnixNano = parsed.UnixNano()
		switch strings.ToLower(strings.TrimSpace(event.Type)) {
		case "turn_started":
			active = nil
			if event.SchemaVersion != "1.0" || strings.TrimSpace(event.SessionID) == "" || strings.TrimSpace(event.ModelID) == "" {
				continue
			}
			active = &sessionTurnBlock{
				StartUnixNano: event.UnixNano,
				SessionID:     event.SessionID,
				ModelID:       strings.TrimSpace(event.ModelID),
				Events:        []sessionEventRecord{event},
			}
		case "turn_ended":
			if active == nil || event.UnixNano <= active.StartUnixNano {
				active = nil
				continue
			}
			active.EndUnixNano = event.UnixNano
			active.Events = append(active.Events, event)
			blocks = append(blocks, *active)
			active = nil
		default:
			if active != nil && event.UnixNano >= active.StartUnixNano {
				active.Events = append(active.Events, event)
			}
		}
	}
	return blocks, scanner.Err()
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
	eventTurns []sessionTurnBlock,
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
	var eventTurn *sessionTurnBlock
	if len(responses) == 0 && terminalRecord != nil {
		if selected, ok := matchingSessionTurnBlock(eventTurns, options.SessionID, start, end); ok {
			eventTurn = selected
			start = minInt64(start, selected.StartUnixNano)
			end = maxInt64(end, selected.EndUnixNano)
		}
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
	if eventTurn != nil {
		extra["timing.source"] = "grok_events_and_hooks"
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
		if turn.Usage.InputTokens > 0 {
			turn.ExtraAttributes["usage_input_tokens"] = turn.Usage.InputTokens
		}
		if turn.Usage.OutputTokens > 0 {
			turn.ExtraAttributes["usage_output_tokens"] = turn.Usage.OutputTokens
		}
	}
	if options.CaptureContent != "none" {
		turn.InputMessages = textMessage("user", prompt, options.MaxChars)
		turn.OutputMessages = textMessage("assistant", output, options.MaxChars)
		turn.InputPreview = preview.Text(prompt, options.MaxChars)
		turn.OutputPreview = preview.Text(output, options.MaxChars)
	}

	toolTriggers := map[string]string{}
	for index, response := range responses {
		call := responseCall(response, options.TurnID, index, start, end)
		turn.LLMCalls = append(turn.LLMCalls, call)
	}
	if len(turn.LLMCalls) > 0 {
		toolTriggers = toolTriggersFromExactResponses(turn.LLMCalls, tools, end)
	}
	if len(turn.LLMCalls) == 0 && terminalRecord != nil {
		if calls, triggers, ok := callsFromSessionEvents(eventTurn, *terminalRecord, options.TurnID, start, end, tools); ok {
			turn.LLMCalls = append(turn.LLMCalls, calls...)
			toolTriggers = triggers
		} else if call, ok := singleCallFromTurnUsage(*terminalRecord, options.TurnID, start, end); ok {
			turn.LLMCalls = append(turn.LLMCalls, call)
		} else if calls, triggers, ok := callsFromTurnUsageAndToolClusters(*terminalRecord, options.TurnID, start, end, tools); ok {
			turn.LLMCalls = append(turn.LLMCalls, calls...)
			toolTriggers = triggers
		}
	}
	if terminalRecord != nil {
		enrichSingleCallFromTurn(&turn, *terminalRecord)
	}

	for _, raw := range tools {
		toolStart, toolEnd, timingSource := toolWindow(raw, start, end)
		arguments := firstNonNil(raw.Pre.Payload["toolInput"], raw.Post.Payload["toolInput"], raw.Pre.Payload["tool_input"], raw.Post.Payload["tool_input"])
		result := firstNonNil(raw.Post.Payload["toolResult"], raw.Post.Payload["tool_result"])
		tool := model.ToolCall{
			CallID: raw.ID, TriggeringLLMCall: toolTriggers[raw.ID], Name: firstNonEmpty(raw.Name, eventToolName(raw.Pre.Payload), eventToolName(raw.Post.Payload), "unknown"),
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

func enrichSingleCallFromTurn(turn *model.Turn, record transcriptRecord) {
	if turn == nil || len(turn.LLMCalls) != 1 || !completeSingleCallUsage(record) {
		return
	}
	call := &turn.LLMCalls[0]
	if turn.Usage != (model.Usage{}) {
		call.Usage = turn.Usage
	}
	if call.InputMessages == nil {
		call.InputMessages = turn.InputMessages
	}
	if call.OutputMessages == nil {
		call.OutputMessages = turn.OutputMessages
	}
	if call.InputPreview == "" {
		call.InputPreview = turn.InputPreview
	}
	if call.OutputPreview == "" {
		call.OutputPreview = turn.OutputPreview
	}
	if call.OutputKind == "" && turn.OutputLength > 0 {
		call.OutputKind = "text"
	}
}

func completeSingleCallUsage(record transcriptRecord) bool {
	usage, ok := record.Params.Update["usage"].(map[string]any)
	if !ok || boolValue(firstNonNil(usage["usageIsIncomplete"], usage["usage_is_incomplete"])) {
		return false
	}
	return int64Value(firstNonNil(usage["modelCalls"], usage["model_calls"])) == 1
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

func toolTriggersFromExactResponses(calls []model.LLMCall, tools []toolBoundary, parentEnd int64) map[string]string {
	triggers := make(map[string]string, len(tools))
	for _, tool := range tools {
		if tool.Pre.RecordedNano <= 0 || tool.Post.RecordedNano <= tool.Pre.RecordedNano {
			continue
		}
		matched := -1
		for index, call := range calls {
			if !containsStringFold(call.FinishReasons, "tool_use") || call.EndUnixNano <= 0 {
				continue
			}
			upperBound := parentEnd
			if index+1 < len(calls) {
				upperBound = calls[index+1].StartUnixNano
			}
			if upperBound <= call.EndUnixNano || tool.Pre.RecordedNano < call.EndUnixNano || tool.Post.RecordedNano > upperBound {
				continue
			}
			if matched >= 0 {
				matched = -2
				break
			}
			matched = index
		}
		if matched >= 0 {
			triggers[tool.ID] = calls[matched].CallID
		}
	}
	return triggers
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
	return usageModelForCallCount(value, 1)
}

func usageModelForCallCount(value any, expectedCalls int64) (string, bool) {
	models, ok := value.(map[string]any)
	if !ok || len(models) != 1 {
		return "", false
	}
	for name, raw := range models {
		modelUsage, ok := raw.(map[string]any)
		if !ok || int64Value(firstNonNil(modelUsage["modelCalls"], modelUsage["model_calls"])) != expectedCalls {
			return "", false
		}
		return strings.TrimSpace(name), strings.TrimSpace(name) != ""
	}
	return "", false
}

func callsFromSessionEvents(
	selected *sessionTurnBlock,
	record transcriptRecord,
	turnID string,
	parentStart, parentEnd int64,
	tools []toolBoundary,
) ([]model.LLMCall, map[string]string, bool) {
	usageValue, ok := record.Params.Update["usage"].(map[string]any)
	if !ok || boolValue(firstNonNil(usageValue["usageIsIncomplete"], usageValue["usage_is_incomplete"])) {
		return nil, nil, false
	}
	modelCalls := int64Value(firstNonNil(usageValue["modelCalls"], usageValue["model_calls"]))
	if modelCalls <= 0 {
		return nil, nil, false
	}

	if selected == nil || selected.StartUnixNano < parentStart || selected.EndUnixNano > parentEnd || selected.EndUnixNano <= selected.StartUnixNano {
		return nil, nil, false
	}

	type eventCall struct {
		LoopIndex     int64
		StartUnixNano int64
		EndUnixNano   int64
		FirstToken    int64
		FinishReason  string
	}
	eventCalls := make([]eventCall, 0, modelCalls)
	currentLoop := int64(-1)
	loopReady := false
	var active *eventCall
	for _, event := range selected.Events {
		eventType := strings.ToLower(strings.TrimSpace(event.Type))
		if eventType == "loop_started" {
			if active != nil {
				if event.UnixNano <= active.StartUnixNano {
					return nil, nil, false
				}
				active.EndUnixNano = event.UnixNano
				eventCalls = append(eventCalls, *active)
				active = nil
			}
			if loopReady {
				return nil, nil, false
			}
			currentLoop = event.LoopIndex
			loopReady = true
			continue
		}
		if eventType == "phase_changed" {
			switch strings.ToLower(strings.TrimSpace(event.Phase)) {
			case "waiting_for_model":
				if active != nil || currentLoop < 0 || !loopReady {
					return nil, nil, false
				}
				active = &eventCall{LoopIndex: currentLoop, StartUnixNano: event.UnixNano}
				loopReady = false
			case "tool_execution":
				if active != nil {
					if event.UnixNano <= active.StartUnixNano {
						return nil, nil, false
					}
					active.EndUnixNano = event.UnixNano
					active.FinishReason = "tool_use"
					eventCalls = append(eventCalls, *active)
					active = nil
				}
			}
			continue
		}
		if eventType == "first_token" && active != nil && active.FirstToken == 0 && event.UnixNano >= active.StartUnixNano {
			active.FirstToken = event.UnixNano
		}
		if eventType == "turn_ended" && active != nil {
			if event.UnixNano <= active.StartUnixNano {
				return nil, nil, false
			}
			active.EndUnixNano = event.UnixNano
			active.FinishReason = firstNonEmpty(stringValue(record.Params.Update, "stop_reason", "stopReason"), event.Outcome)
			eventCalls = append(eventCalls, *active)
			active = nil
		}
	}
	if active != nil || loopReady || int64(len(eventCalls)) != modelCalls {
		return nil, nil, false
	}

	calls := make([]model.LLMCall, 0, len(eventCalls))
	for index, eventCall := range eventCalls {
		if eventCall.StartUnixNano < parentStart || eventCall.EndUnixNano > parentEnd || eventCall.EndUnixNano <= eventCall.StartUnixNano {
			return nil, nil, false
		}
		ttft := float64(0)
		if eventCall.FirstToken > 0 {
			if eventCall.FirstToken > eventCall.EndUnixNano {
				return nil, nil, false
			}
			ttft = float64(eventCall.FirstToken-eventCall.StartUnixNano) / float64(time.Millisecond)
		}
		calls = append(calls, model.LLMCall{
			CallID:        derivedID(turnID, "llm", "events", strconv.FormatInt(eventCall.LoopIndex, 10), strconv.Itoa(index)),
			StartUnixNano: eventCall.StartUnixNano,
			EndUnixNano:   eventCall.EndUnixNano,
			RequestModel:  selected.ModelID,
			FinishReasons: stringSlice(eventCall.FinishReason),
			TTFTMs:        ttft,
			Status:        "ok",
			ExtraAttributes: map[string]any{
				"timing.source": "grok_events",
			},
		})
	}

	toolTriggers := make(map[string]string, len(tools))
	for _, tool := range tools {
		if tool.Pre.RecordedNano <= 0 || tool.Post.RecordedNano <= tool.Pre.RecordedNano {
			continue
		}
		for index := range calls {
			nextStart := parentEnd
			if index+1 < len(calls) {
				nextStart = calls[index+1].StartUnixNano
			}
			if tool.Pre.RecordedNano >= calls[index].EndUnixNano && tool.Post.RecordedNano <= nextStart {
				toolTriggers[tool.ID] = calls[index].CallID
				break
			}
		}
	}
	return calls, toolTriggers, true
}

func matchingSessionTurnBlock(blocks []sessionTurnBlock, sessionID string, hookStart, hookEnd int64) (*sessionTurnBlock, bool) {
	if strings.TrimSpace(sessionID) == "" || hookStart <= 0 || hookEnd <= hookStart {
		return nil, false
	}
	var selected *sessionTurnBlock
	for index := range blocks {
		block := &blocks[index]
		if block.SessionID != sessionID || block.EndUnixNano <= block.StartUnixNano {
			continue
		}
		if absInt64(block.StartUnixNano-hookStart) > int64(maxSessionEventHookSkew) ||
			absInt64(block.EndUnixNano-hookEnd) > int64(maxSessionEventHookSkew) {
			continue
		}
		if selected != nil {
			return nil, false
		}
		selected = block
	}
	return selected, selected != nil
}

func callsFromTurnUsageAndToolClusters(
	record transcriptRecord,
	turnID string,
	parentStart, parentEnd int64,
	tools []toolBoundary,
) ([]model.LLMCall, map[string]string, bool) {
	usageValue, ok := record.Params.Update["usage"].(map[string]any)
	if !ok || boolValue(firstNonNil(usageValue["usageIsIncomplete"], usageValue["usage_is_incomplete"])) {
		return nil, nil, false
	}
	usage := promptUsage(usageValue)
	if usage == (model.Usage{}) {
		return nil, nil, false
	}
	modelCalls := int64Value(firstNonNil(usageValue["modelCalls"], usageValue["model_calls"]))
	if modelCalls <= 1 {
		return nil, nil, false
	}
	modelName, ok := usageModelForCallCount(firstNonNil(usageValue["modelUsage"], usageValue["model_usage"]), modelCalls)
	if !ok {
		return nil, nil, false
	}
	durationMS := int64Value(firstNonNil(usageValue["apiDurationMs"], usageValue["api_duration_ms"]))
	if durationMS <= 0 || parentEnd <= parentStart || durationMS > (parentEnd-parentStart)/int64(time.Millisecond) {
		return nil, nil, false
	}

	clusters, ok := completeToolExecutionClusters(tools, parentStart, parentEnd)
	if !ok || int64(len(clusters)) != modelCalls-1 {
		return nil, nil, false
	}
	gaps := make([][2]int64, 0, modelCalls)
	gapStart := parentStart
	for _, cluster := range clusters {
		if cluster.StartUnixNano <= gapStart {
			return nil, nil, false
		}
		gaps = append(gaps, [2]int64{gapStart, cluster.StartUnixNano})
		gapStart = cluster.EndUnixNano
	}
	if gapStart >= parentEnd {
		return nil, nil, false
	}
	gaps = append(gaps, [2]int64{gapStart, parentEnd})

	durations, ok := fitDurationAcrossGaps(durationMS*int64(time.Millisecond), gaps)
	if !ok {
		return nil, nil, false
	}
	calls := make([]model.LLMCall, 0, len(gaps))
	toolTriggers := make(map[string]string, len(tools))
	terminalReason := stringValue(record.Params.Update, "stop_reason", "stopReason")
	for index, gap := range gaps {
		callID := derivedID(turnID, "llm", "hook-boundary", strconv.Itoa(index))
		finishReason := terminalReason
		if index < len(clusters) {
			finishReason = "tool_use"
			for _, toolID := range clusters[index].ToolIDs {
				toolTriggers[toolID] = callID
			}
		}
		calls = append(calls, model.LLMCall{
			CallID:        callID,
			StartUnixNano: gap[1] - durations[index],
			EndUnixNano:   gap[1],
			ResponseModel: modelName,
			FinishReasons: stringSlice(finishReason),
			Status:        "ok",
			ExtraAttributes: map[string]any{
				"timing.source":           "grok_hook_boundaries",
				"gtrace.synthetic":        true,
				"gtrace.timing.estimated": true,
			},
		})
	}
	return calls, toolTriggers, true
}

func completeToolExecutionClusters(tools []toolBoundary, parentStart, parentEnd int64) ([]toolExecutionCluster, bool) {
	if len(tools) == 0 {
		return nil, false
	}
	intervals := make([]toolExecutionCluster, 0, len(tools))
	for _, tool := range tools {
		if tool.Pre.RecordedNano <= 0 || tool.Post.RecordedNano <= tool.Pre.RecordedNano {
			return nil, false
		}
		start := tool.Pre.RecordedNano
		end := tool.Post.RecordedNano
		durationMS := int64Value(firstNonNil(tool.Post.Payload["durationMs"], tool.Post.Payload["duration_ms"]))
		if durationMS > 0 {
			if end <= parentStart || durationMS > (end-parentStart)/int64(time.Millisecond) {
				return nil, false
			}
			start = end - durationMS*int64(time.Millisecond)
		}
		if start < parentStart || end > parentEnd || end <= start {
			return nil, false
		}
		intervals = append(intervals, toolExecutionCluster{
			StartUnixNano: start,
			EndUnixNano:   end,
			ToolIDs:       []string{tool.ID},
		})
	}
	sort.Slice(intervals, func(left, right int) bool {
		if intervals[left].StartUnixNano != intervals[right].StartUnixNano {
			return intervals[left].StartUnixNano < intervals[right].StartUnixNano
		}
		if intervals[left].EndUnixNano != intervals[right].EndUnixNano {
			return intervals[left].EndUnixNano < intervals[right].EndUnixNano
		}
		return intervals[left].ToolIDs[0] < intervals[right].ToolIDs[0]
	})

	clusters := make([]toolExecutionCluster, 0, len(intervals))
	for _, interval := range intervals {
		if len(clusters) == 0 || interval.StartUnixNano > clusters[len(clusters)-1].EndUnixNano {
			clusters = append(clusters, interval)
			continue
		}
		cluster := &clusters[len(clusters)-1]
		if interval.EndUnixNano > cluster.EndUnixNano {
			cluster.EndUnixNano = interval.EndUnixNano
		}
		cluster.ToolIDs = append(cluster.ToolIDs, interval.ToolIDs...)
	}
	return clusters, true
}

func fitDurationAcrossGaps(totalDuration int64, gaps [][2]int64) ([]int64, bool) {
	if totalDuration <= 0 || len(gaps) == 0 || totalDuration < int64(len(gaps)) {
		return nil, false
	}
	capacities := make([]int64, len(gaps))
	suffixCapacity := make([]int64, len(gaps)+1)
	for index := len(gaps) - 1; index >= 0; index-- {
		capacity := gaps[index][1] - gaps[index][0]
		if capacity <= 0 || suffixCapacity[index+1] > int64(^uint64(0)>>1)-capacity {
			return nil, false
		}
		capacities[index] = capacity
		suffixCapacity[index] = suffixCapacity[index+1] + capacity
	}
	if totalDuration > suffixCapacity[0] {
		return nil, false
	}

	durations := make([]int64, len(gaps))
	remaining := totalDuration
	for index, capacity := range capacities {
		remainingGaps := int64(len(gaps) - index)
		duration := remaining / remainingGaps
		minimumForRest := remainingGaps - 1
		minimumHere := remaining - suffixCapacity[index+1]
		if minimumHere < 1 {
			minimumHere = 1
		}
		maximumHere := remaining - minimumForRest
		if maximumHere > capacity {
			maximumHere = capacity
		}
		if duration < minimumHere {
			duration = minimumHere
		}
		if duration > maximumHere {
			duration = maximumHere
		}
		if duration <= 0 || duration > capacity {
			return nil, false
		}
		durations[index] = duration
		remaining -= duration
	}
	if remaining != 0 {
		return nil, false
	}
	return durations, true
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

func containsStringFold(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(expected)) {
			return true
		}
	}
	return false
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

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func derivedID(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:12])
}
