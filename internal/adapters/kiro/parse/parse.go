package parse

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	SessionDir         string
	SessionID          string
	Cwd                string
	AssistantResponse  string
	AgentVersion       string
	CaptureContent     string
	MaxChars           int
	ResourceAttributes map[string]any
	Events             []JournalEvent
}

type rawTool struct {
	ID        string
	Name      string
	Arguments any
}

type rawAssistant struct {
	ID    string
	Text  string
	Tools []rawTool
}

type rawTurn struct {
	Prompt     string
	Assistants []rawAssistant
	Results    map[string]any
}

type sessionCandidate struct {
	Sidecar   map[string]any
	ID        string
	JSONLPath string
	Updated   int64
}

func ReadLatestTurn(options Options) (model.Turn, bool, error) {
	modern, found, err := findModernSession(options.SessionDir, options.SessionID)
	if err != nil {
		return model.Turn{}, false, err
	}
	if found {
		return readModernTurn(modern, options)
	}
	candidate, err := findSession(options.SessionDir, options.SessionID)
	if err != nil {
		return model.Turn{}, false, err
	}
	turns, err := readSessionLines(candidate.JSONLPath)
	if err != nil {
		return model.Turn{}, false, err
	}
	if len(turns) == 0 {
		return model.Turn{}, false, nil
	}
	turnIndex := len(turns) - 1
	expectedPrompt := latestEventString(options.Events, "UserPromptSubmit", "prompt")
	if expectedPrompt != "" {
		for index := len(turns) - 1; index >= 0; index-- {
			if strings.TrimSpace(turns[index].Prompt) == strings.TrimSpace(expectedPrompt) {
				turnIndex = index
				break
			}
		}
	}
	raw := turns[turnIndex]
	if strings.TrimSpace(raw.Prompt) == "" {
		raw.Prompt = latestEventString(options.Events, "UserPromptSubmit", "prompt")
	}
	if !hasAssistantOutput(raw) && strings.TrimSpace(options.AssistantResponse) == "" {
		return model.Turn{}, false, nil
	}
	metadatas := objectSlice(nested(candidate.Sidecar, "session_state", "conversation_metadata", "user_turn_metadatas"))
	var metadata map[string]any
	if len(metadatas) > 0 {
		index := turnIndex
		if index >= len(metadatas) {
			index = len(metadatas) - 1
		}
		metadata = metadatas[index]
	}
	turn := normalize(candidate, raw, metadata, options)
	if turn.SessionID == "" || turn.TurnID == "" || turn.FinalStatus == model.FinalStatusUnset {
		return model.Turn{}, false, nil
	}
	return turn, true, nil
}

func findSession(sessionDir, sessionID string) (sessionCandidate, error) {
	if strings.TrimSpace(sessionDir) == "" {
		return sessionCandidate{}, errors.New("Kiro session directory is empty")
	}
	if !safeID(sessionID) {
		return sessionCandidate{}, os.ErrNotExist
	}
	for _, directory := range legacySessionDirs(sessionDir) {
		sidecarPath := filepath.Join(directory, sessionID+".json")
		jsonlPath := filepath.Join(directory, sessionID+".jsonl")
		value, err := readObject(sidecarPath)
		if err != nil {
			continue
		}
		if storedID := stringValue(value, "session_id"); storedID != "" && storedID != sessionID {
			continue
		}
		if _, err := os.Stat(jsonlPath); err != nil {
			continue
		}
		return sessionCandidate{
			Sidecar: value, ID: sessionID, JSONLPath: jsonlPath,
			Updated: parseTime(value["updated_at"]),
		}, nil
	}
	return sessionCandidate{}, os.ErrNotExist
}

func legacySessionDirs(sessionDir string) []string {
	directories := []string{filepath.Clean(sessionDir)}
	if filepath.Base(filepath.Clean(sessionDir)) != "cli" {
		directories = append(directories, filepath.Join(sessionDir, "cli"))
	}
	return directories
}

func readSessionLines(path string) ([]rawTurn, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	turns := make([]rawTurn, 0)
	var current *rawTurn
	results := map[string]any{}
	flush := func() {
		if current == nil {
			return
		}
		current.Results = copyMap(results)
		turns = append(turns, *current)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var value map[string]any
		if json.Unmarshal([]byte(line), &value) != nil {
			continue
		}
		kind := stringValue(value, "kind")
		data := object(value["data"])
		switch kind {
		case "Prompt":
			flush()
			results = map[string]any{}
			current = &rawTurn{Prompt: textContent(data["content"])}
		case "AssistantMessage":
			if current == nil {
				continue
			}
			assistant := rawAssistant{ID: stringValue(data, "message_id")}
			for _, item := range objectSlice(data["content"]) {
				switch stringValue(item, "kind") {
				case "text":
					assistant.Text += scalarText(item["data"])
				case "toolUse":
					toolData := object(item["data"])
					assistant.Tools = append(assistant.Tools, rawTool{
						ID: stringValue(toolData, "toolUseId", "tool_use_id", "id"), Name: normalizedToolName(stringValue(toolData, "name", "tool_name")), Arguments: toolData["input"],
					})
				}
			}
			current.Assistants = append(current.Assistants, assistant)
		case "ToolResults":
			for _, item := range objectSlice(data["content"]) {
				if stringValue(item, "kind") != "toolResult" {
					continue
				}
				resultData := object(item["data"])
				id := stringValue(resultData, "toolUseId", "tool_use_id", "id")
				if id != "" {
					results[id] = resultContent(resultData["content"])
				}
			}
		}
	}
	flush()
	return turns, scanner.Err()
}

func normalize(candidate sessionCandidate, raw rawTurn, metadata map[string]any, options Options) model.Turn {
	sessionID := firstNonEmpty(options.SessionID, stringValue(candidate.Sidecar, "session_id"), candidate.ID)
	end := parseTime(metadata["end_timestamp"])
	if end == 0 {
		end = latestEventTime(options.Events, "Stop")
	}
	if end == 0 {
		end = candidate.Updated
	}
	if end == 0 {
		end = time.Now().UnixNano()
	}
	duration := durationNano(metadata["turn_duration"])
	start := end - duration
	if duration <= 0 {
		start = latestEventTime(options.Events, "UserPromptSubmit")
	}
	if start <= 0 || start >= end {
		start = end - int64(time.Millisecond)
	}
	turnID := turnID(metadata)
	if turnID == "" && len(raw.Assistants) > 0 {
		turnID = raw.Assistants[0].ID
	}
	if turnID == "" {
		turnID = derivedID(sessionID, fmt.Sprintf("%d", start), raw.Prompt)
	}
	modelName := stringValue(metadata, "model")
	if modelName == "" {
		modelName = stringValue(object(nested(candidate.Sidecar, "session_state", "rts_model_state", "model_info")), "model_id")
	}
	usage := model.Usage{
		InputTokens:       int64Value(metadata["input_token_count"]),
		OutputTokens:      int64Value(metadata["output_token_count"]),
		CacheReadTokens:   int64Value(metadata["cache_read_input_token_count"]),
		CacheCreateTokens: int64Value(metadata["cache_write_input_token_count"]),
	}
	status, errorType, reason := finalStatus(stringValue(metadata, "end_reason"))
	output := latestAssistantText(raw)
	if output == "" {
		output = options.AssistantResponse
	}
	resource := copyMap(options.ResourceAttributes)
	resource["agent_runtime"] = "kiro"
	resource["telemetry.sdk.language"] = "go"
	resource["telemetry.sdk.name"] = "gtrace"
	turn := model.Turn{
		SessionID: sessionID, TurnID: turnID, AgentRuntime: "kiro", AgentName: "Kiro", AgentVersion: options.AgentVersion,
		StartUnixNano: start, EndUnixNano: end, FinalStatus: status, InputLength: len([]rune(raw.Prompt)), OutputLength: len([]rune(output)),
		Usage: usage, Resource: resource, ErrorType: errorType, Reason: reason,
		ExtraAttributes: map[string]any{"request_type": "user_request", "timing.source": "kiro_turn_metadata"},
	}
	if options.CaptureContent != "none" {
		turn.InputMessages = textMessage("user", raw.Prompt, options.MaxChars)
		turn.OutputMessages = textMessage("assistant", output, options.MaxChars)
		turn.InputPreview = preview.Text(raw.Prompt, options.MaxChars)
		turn.OutputPreview = preview.Text(output, options.MaxChars)
	}

	preEvents, postEvents := toolEvents(options.Events)
	assistantCount := len(raw.Assistants)
	if assistantCount == 0 && output != "" {
		raw.Assistants = append(raw.Assistants, rawAssistant{ID: derivedID(turnID, "assistant"), Text: output})
		assistantCount = 1
	}
	lastToolEnd := start
	toolPosition := 0
	for index, assistant := range raw.Assistants {
		callStart, callEnd := sliceWindow(start, end, index, assistantCount)
		if lastToolEnd > callStart {
			callStart = lastToolEnd
		}
		call := model.LLMCall{
			CallID: firstNonEmpty(assistant.ID, derivedID(turnID, "llm", strconv.Itoa(index))), StartUnixNano: callStart, EndUnixNano: callEnd,
			Provider: providerForModel(modelName), RequestModel: modelName, ResponseModel: modelName,
			FinishReasons: []string{"stop"}, Status: statusValue(errorType), ErrorType: errorType, Reason: reason,
			ExtraAttributes: map[string]any{"timing.source": "kiro_turn_slice"},
		}
		if assistantCount == 1 {
			call.Usage = usage
		}
		if options.CaptureContent != "none" {
			call.InputMessages = inputForAssistant(raw, index, options.MaxChars)
			call.OutputMessages = assistantMessages(assistant, options.MaxChars)
			call.InputPreview = preview.Text(call.InputMessages, options.MaxChars)
			call.OutputPreview = preview.Text(call.OutputMessages, options.MaxChars)
			call.OutputKind = "text"
			if len(assistant.Tools) > 0 {
				call.OutputKind = "tool_call"
			}
		}
		for _, rawTool := range assistant.Tools {
			pre, post := matchToolEvent(rawTool, preEvents, postEvents, toolPosition)
			toolPosition++
			toolStart := pre.RecordedNano
			if toolStart <= 0 {
				toolStart = callEnd
			}
			if toolStart > callStart {
				call.EndUnixNano = toolStart
			}
			toolEnd := post.RecordedNano
			if toolEnd <= toolStart {
				toolEnd = toolStart + 1
			}
			lastToolEnd = maxInt64(lastToolEnd, toolEnd)
			result := raw.Results[rawTool.ID]
			if result == nil {
				result = firstNonNil(post.Payload["tool_response"], post.Payload["tool_output"])
			}
			tool := model.ToolCall{
				CallID:            firstNonEmpty(rawTool.ID, eventToolID(pre.Payload), derivedID(turnID, "tool", strconv.Itoa(toolPosition))),
				TriggeringLLMCall: call.CallID, Name: firstNonEmpty(rawTool.Name, eventToolName(pre.Payload), "unknown"),
				StartUnixNano: toolStart, EndUnixNano: toolEnd, Status: "ok", ResultStatus: "completed",
				ExtraAttributes: map[string]any{"timing.source": toolTimingSource(pre)},
			}
			if post.RecordedNano == 0 {
				tool.Status, tool.ResultStatus = "unset", "unset"
			}
			if boolValue(post.Payload["is_error"]) || boolValue(post.Payload["isError"]) {
				tool.Status, tool.ResultStatus, tool.ErrorType = "error", "error", "tool_error"
			}
			if options.CaptureContent != "none" {
				arguments := firstNonNil(rawTool.Arguments, pre.Payload["tool_input"])
				tool.Arguments = privacy.Sanitize(arguments, options.MaxChars)
				tool.Result = privacy.Sanitize(result, options.MaxChars)
				tool.InputPreview = preview.Text(arguments, options.MaxChars)
				tool.OutputPreview = preview.Text(result, options.MaxChars)
				tool.Command = commandValue(arguments, options.MaxChars)
			}
			turn.ToolCalls = append(turn.ToolCalls, tool)
		}
		turn.LLMCalls = append(turn.LLMCalls, call)
	}
	if output != "" {
		assistantStart := end - 1
		if assistantStart < start {
			assistantStart = start
		}
		assistantOutput := model.AssistantOutput{
			StartUnixNano: assistantStart, EndUnixNano: end, OutputKind: "text", Provider: providerForModel(modelName),
			RequestModel: modelName, ResponseModel: modelName, Status: statusValue(errorType), ErrorType: errorType, Reason: reason,
			ExtraAttributes: map[string]any{"timing.source": "kiro_turn_metadata"},
		}
		if options.CaptureContent != "none" {
			assistantOutput.OutputMessages = turn.OutputMessages
			assistantOutput.OutputPreview = turn.OutputPreview
		}
		turn.AssistantOutputs = append(turn.AssistantOutputs, assistantOutput)
	}
	return turn
}

func readObject(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func turnID(metadata map[string]any) string {
	if result := object(nested(metadata, "result", "Ok")); len(result) > 0 {
		if id := stringValue(result, "id"); id != "" {
			return id
		}
	}
	for _, value := range anySlice(metadata["message_ids"]) {
		if id := scalarText(value); id != "" {
			return id
		}
	}
	loop := object(metadata["loop_id"])
	if randValue := scalarText(loop["rand"]); randValue != "" {
		return derivedID(stringValue(object(loop["agent_id"]), "name"), randValue)
	}
	return ""
}

func finalStatus(reason string) (model.FinalStatus, string, string) {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.Contains(normalized, "cancel"), strings.Contains(normalized, "interrupt"), strings.Contains(normalized, "abort"):
		return model.FinalStatusCancelled, "", reason
	case strings.Contains(normalized, "error"), strings.Contains(normalized, "fail"):
		return model.FinalStatusCompleted, "kiro_agent_error", reason
	default:
		return model.FinalStatusCompleted, "", ""
	}
}

func hasAssistantOutput(turn rawTurn) bool {
	for _, assistant := range turn.Assistants {
		if strings.TrimSpace(assistant.Text) != "" || len(assistant.Tools) > 0 {
			return true
		}
	}
	return false
}

func latestAssistantText(turn rawTurn) string {
	for index := len(turn.Assistants) - 1; index >= 0; index-- {
		if text := strings.TrimSpace(turn.Assistants[index].Text); text != "" {
			return text
		}
	}
	return ""
}

func toolEvents(events []JournalEvent) ([]JournalEvent, []JournalEvent) {
	pre := make([]JournalEvent, 0)
	post := make([]JournalEvent, 0)
	for _, event := range events {
		switch strings.ToLower(event.Event) {
		case "pretooluse":
			pre = append(pre, event)
		case "posttooluse":
			post = append(post, event)
		}
	}
	return pre, post
}

func matchToolEvent(tool rawTool, pre, post []JournalEvent, fallback int) (JournalEvent, JournalEvent) {
	id := tool.ID
	for _, before := range pre {
		if id != "" && eventToolID(before.Payload) == id {
			return before, matchingPost(before, post)
		}
	}
	for _, before := range pre {
		if normalizedToolName(eventToolName(before.Payload)) == normalizedToolName(tool.Name) {
			return before, matchingPost(before, post)
		}
	}
	if fallback >= 0 && fallback < len(pre) {
		return pre[fallback], matchingPost(pre[fallback], post)
	}
	return JournalEvent{}, JournalEvent{}
}

func matchingPost(pre JournalEvent, post []JournalEvent) JournalEvent {
	id := eventToolID(pre.Payload)
	for _, event := range post {
		if id != "" && eventToolID(event.Payload) == id {
			return event
		}
	}
	name := normalizedToolName(eventToolName(pre.Payload))
	for _, event := range post {
		if event.RecordedNano >= pre.RecordedNano && normalizedToolName(eventToolName(event.Payload)) == name {
			return event
		}
	}
	return JournalEvent{}
}

func latestEventString(events []JournalEvent, eventName, key string) string {
	for index := len(events) - 1; index >= 0; index-- {
		if strings.EqualFold(events[index].Event, eventName) {
			return stringValue(events[index].Payload, key)
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

func inputForAssistant(turn rawTurn, index, maxChars int) any {
	if index == 0 {
		return textMessage("user", turn.Prompt, maxChars)
	}
	parts := make([]any, 0)
	for _, tool := range turn.Assistants[index-1].Tools {
		if result := turn.Results[tool.ID]; result != nil {
			parts = append(parts, map[string]any{"type": "tool_result", "tool_call_id": tool.ID, "content": privacy.Sanitize(result, maxChars)})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []any{map[string]any{"role": "tool", "parts": parts}}
}

func assistantMessages(assistant rawAssistant, maxChars int) any {
	parts := make([]any, 0, len(assistant.Tools)+1)
	if assistant.Text != "" {
		parts = append(parts, map[string]any{"type": "text", "content": privacy.Text(assistant.Text, maxChars)})
	}
	for _, tool := range assistant.Tools {
		parts = append(parts, map[string]any{"type": "tool_call", "tool_call_id": tool.ID, "name": tool.Name, "arguments": privacy.Sanitize(tool.Arguments, maxChars)})
	}
	if len(parts) == 0 {
		return nil
	}
	return []any{map[string]any{"role": "assistant", "parts": parts}}
}

func textMessage(role, text string, maxChars int) any {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []any{map[string]any{"role": role, "parts": []any{map[string]any{"type": "text", "content": privacy.Text(text, maxChars)}}}}
}

func textContent(value any) string {
	var result strings.Builder
	for _, item := range objectSlice(value) {
		if stringValue(item, "kind") == "text" {
			result.WriteString(scalarText(item["data"]))
		}
	}
	return result.String()
}

func resultContent(value any) any {
	parts := make([]any, 0)
	for _, item := range objectSlice(value) {
		switch stringValue(item, "kind") {
		case "text":
			parts = append(parts, scalarText(item["data"]))
		case "json":
			parts = append(parts, item["data"])
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return parts
}

func commandValue(value any, maxChars int) string {
	current := object(value)
	for _, key := range []string{"command", "cmd", "script"} {
		if text := stringValue(current, key); text != "" {
			return privacy.Text(text, maxChars)
		}
	}
	return ""
}

func eventToolID(value map[string]any) string {
	return stringValue(value, "tool_use_id", "toolUseId", "tool_call_id", "toolCallId", "id")
}

func eventToolName(value map[string]any) string {
	return stringValue(value, "tool_name", "toolName", "name")
}

func normalizedToolName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "read":
		return "fs_read"
	case "write":
		return "fs_write"
	case "shell":
		return "execute_bash"
	default:
		return strings.TrimSpace(value)
	}
}

func providerForModel(value string) string {
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "claude"):
		return "anthropic"
	case strings.Contains(lower, "nova"), strings.Contains(lower, "bedrock"):
		return "aws"
	default:
		return ""
	}
}

func sliceWindow(start, end int64, index, count int) (int64, int64) {
	if count <= 0 || end <= start {
		return start, end
	}
	width := (end - start) / int64(count)
	if width <= 0 {
		width = 1
	}
	currentStart := start + int64(index)*width
	currentEnd := currentStart + width
	if index == count-1 || currentEnd > end {
		currentEnd = end
	}
	if currentEnd <= currentStart {
		currentEnd = currentStart + 1
	}
	return currentStart, currentEnd
}

func durationNano(value any) int64 {
	current := object(value)
	return int64Value(current["secs"])*int64(time.Second) + int64Value(current["nanos"])
}

func parseTime(value any) int64 {
	switch current := value.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, current); err == nil {
			return parsed.UnixNano()
		}
	case float64:
		if current > 1e15 {
			return int64(current)
		}
		if current > 1e12 {
			return int64(current * float64(time.Millisecond))
		}
	}
	return 0
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

func safeID(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 200 {
		return false
	}
	return !strings.ContainsAny(value, `/\`)
}

func nested(value map[string]any, keys ...string) any {
	var current any = value
	for _, key := range keys {
		objectValue, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = objectValue[key]
	}
	return current
}

func object(value any) map[string]any {
	current, _ := value.(map[string]any)
	if current == nil {
		return map[string]any{}
	}
	return current
}

func objectSlice(value any) []map[string]any {
	out := make([]map[string]any, 0)
	for _, item := range anySlice(value) {
		if current, ok := item.(map[string]any); ok {
			out = append(out, current)
		}
	}
	return out
}

func anySlice(value any) []any {
	current, _ := value.([]any)
	return current
}

func stringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := scalarText(value[key]); text != "" {
			return text
		}
	}
	return ""
}

func scalarText(value any) string {
	switch current := value.(type) {
	case string:
		return strings.TrimSpace(current)
	case float64:
		return strconv.FormatFloat(current, 'f', -1, 64)
	case json.Number:
		return current.String()
	}
	return ""
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

func boolValue(value any) bool {
	switch current := value.(type) {
	case bool:
		return current
	case string:
		parsed, _ := strconv.ParseBool(current)
		return parsed
	}
	return false
}

func statusValue(errorType string) string {
	if errorType != "" {
		return "error"
	}
	return "ok"
}

func toolTimingSource(value JournalEvent) string {
	if value.RecordedNano > 0 {
		return "kiro_hook"
	}
	return "kiro_turn_slice"
}

func derivedID(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
