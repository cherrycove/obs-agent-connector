package parse

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	LastAssistant      string
	AgentVersion       string
	CaptureContent     string
	MaxChars           int
	ResourceAttributes map[string]any
	Events             []JournalEvent
}

type transcriptRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Sequence      int    `json:"sequence"`
	RecordID      string `json:"record_id"`
	Timestamp     string `json:"timestamp"`
	ThreadID      string `json:"thread_id"`
	AgentID       string `json:"agent_id"`
	Role          string `json:"role"`
	MessageID     string `json:"message_id"`
	Content       any    `json:"content"`
	Name          string `json:"name"`
}

type rawAssistant struct {
	ID        string
	Content   any
	Text      string
	ToolCount int
}

type toolBoundary struct {
	ID          string
	Name        string
	Pre         JournalEvent
	Post        JournalEvent
	Failure     bool
	Interrupted bool
	AgentID     string
	AgentName   string
}

func ReadTurn(options Options) (model.Turn, bool, error) {
	if strings.TrimSpace(options.SessionID) == "" {
		return model.Turn{}, false, errors.New("dcode session_id is empty")
	}
	if strings.TrimSpace(options.TranscriptPath) == "" {
		return model.Turn{}, false, errors.New("dcode transcript_path is empty")
	}
	records, err := readTranscript(options.TranscriptPath)
	if err != nil {
		return model.Turn{}, false, err
	}
	prompt := latestEventString(options.Events, "UserPromptSubmit", "prompt")
	transcriptPrompt, assistants := currentTurn(records, prompt)
	if prompt == "" {
		prompt = transcriptPrompt
	}
	tools := collectTools(options.Events)
	output := strings.TrimSpace(options.LastAssistant)
	if output == "" {
		for index := len(assistants) - 1; index >= 0; index-- {
			if text := strings.TrimSpace(assistants[index].Text); text != "" {
				output = text
				break
			}
		}
	}
	if len(assistants) == 0 && output != "" {
		assistants = append(assistants, rawAssistant{ID: derivedID(options.TurnID, "assistant"), Content: output, Text: output})
	}
	_, failedSession := failedSessionEnd(options.Events)
	if strings.TrimSpace(prompt) == "" || (len(assistants) == 0 && len(tools) == 0 && output == "" && !failedSession) {
		return model.Turn{}, false, nil
	}
	turn := normalize(options, prompt, output, assistants, tools)
	if turn.TurnID == "" || turn.FinalStatus == model.FinalStatusUnset {
		return model.Turn{}, false, nil
	}
	return turn, true, nil
}

func readTranscript(path string) ([]transcriptRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records := make([]transcriptRecord, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var record transcriptRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.SchemaVersion != 1 {
			continue
		}
		if record.ThreadID != "" && record.ThreadID != recordsThreadID(records, record.ThreadID) {
			continue
		}
		records = append(records, record)
	}
	return records, scanner.Err()
}

func recordsThreadID(records []transcriptRecord, fallback string) string {
	for _, record := range records {
		if record.ThreadID != "" {
			return record.ThreadID
		}
	}
	return fallback
}

func currentTurn(records []transcriptRecord, expectedPrompt string) (string, []rawAssistant) {
	start := -1
	if expectedPrompt != "" {
		for index := len(records) - 1; index >= 0; index-- {
			if records[index].Role == "user" && equivalentText(contentText(records[index].Content), expectedPrompt) {
				start = index
				break
			}
		}
	}
	if start < 0 {
		for index := len(records) - 1; index >= 0; index-- {
			if records[index].Role == "user" {
				start = index
				break
			}
		}
	}
	if start < 0 {
		return "", nil
	}
	prompt := contentText(records[start].Content)
	assistants := make([]rawAssistant, 0)
	for _, record := range records[start+1:] {
		if record.Role == "user" {
			break
		}
		if record.AgentID != "" {
			continue
		}
		switch record.Role {
		case "assistant":
			assistants = append(assistants, rawAssistant{
				ID: firstNonEmpty(record.MessageID, record.RecordID), Content: record.Content, Text: contentText(record.Content),
			})
		case "tool":
			if len(assistants) > 0 {
				assistants[len(assistants)-1].ToolCount++
			}
		}
	}
	return prompt, assistants
}

func collectTools(events []JournalEvent) []toolBoundary {
	byID := map[string]*toolBoundary{}
	order := make([]string, 0)
	ensure := func(id, name string, position int) *toolBoundary {
		if id == "" {
			id = derivedID("dcode-tool", name, strconv.Itoa(position))
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
		name := strings.ToLower(strings.TrimSpace(event.Event))
		payload := event.Payload
		switch name {
		case "pretooluse":
			value := ensure(eventToolID(payload), eventToolName(payload), index)
			value.Pre = event
		case "posttooluse", "posttoolusefailure":
			value := ensure(eventToolID(payload), eventToolName(payload), index)
			value.Post = event
			value.Failure = name == "posttoolusefailure"
			value.Interrupted = value.Failure && boolValue(firstNonNil(payload["is_interrupt"], payload["isInterrupt"]))
		case "subagentstart", "subagentstop":
			id := stringValue(payload, "agent_id", "agentId")
			if id == "" {
				continue
			}
			value := ensure(id, "task", index)
			value.AgentID = id
			value.AgentName = stringValue(payload, "agent_type", "agentType")
		}
	}
	out := make([]toolBoundary, 0, len(order))
	for _, id := range order {
		value := byID[id]
		if value.Pre.RecordedNano == 0 && value.Post.RecordedNano == 0 {
			continue
		}
		out = append(out, *value)
	}
	return out
}

func normalize(options Options, prompt, output string, assistants []rawAssistant, tools []toolBoundary) model.Turn {
	start := latestEventTime(options.Events, "UserPromptSubmit")
	end := latestEventTime(options.Events, "Stop")
	sessionEnd, failedSession := failedSessionEnd(options.Events)
	if end <= 0 && failedSession {
		end = sessionEnd.RecordedNano
	}
	if end <= 0 {
		end = time.Now().UnixNano()
	}
	if start <= 0 || start >= end {
		start = end - int64(time.Millisecond)
	}
	turnID := strings.TrimSpace(options.TurnID)
	if turnID == "" {
		turnID = derivedID(options.SessionID, fmt.Sprintf("%d", start), prompt)
	}
	resource := copyMap(options.ResourceAttributes)
	resource["agent_runtime"] = "dcode"
	resource["telemetry.sdk.language"] = "go"
	resource["telemetry.sdk.name"] = "gtrace"
	errorType := ""
	reason := ""
	extraAttributes := map[string]any{"request_type": "user_request", "timing.source": "dcode_hooks"}
	if failedSession {
		errorType = "dcode_agent_error"
		reason = "Dcode ended the session before emitting Stop"
		extraAttributes["dcode.session_end.reason"] = stringValue(sessionEnd.Payload, "reason")
	}
	turn := model.Turn{
		SessionID: options.SessionID, TurnID: turnID, AgentRuntime: "dcode",
		AgentName: "Deep Agents Code", AgentVersion: options.AgentVersion,
		StartUnixNano: start, EndUnixNano: end, FinalStatus: model.FinalStatusCompleted,
		InputLength: len([]rune(prompt)), OutputLength: len([]rune(output)), Resource: resource,
		ExtraAttributes: extraAttributes, ErrorType: errorType, Reason: reason,
	}
	if options.CaptureContent != "none" {
		turn.InputMessages = textMessage("user", prompt, options.MaxChars)
		turn.OutputMessages = textMessage("assistant", output, options.MaxChars)
		turn.InputPreview = preview.Text(prompt, options.MaxChars)
		turn.OutputPreview = preview.Text(output, options.MaxChars)
	}

	for index, assistant := range assistants {
		callStart, callEnd := sliceWindow(start, end, index, len(assistants))
		call := model.LLMCall{
			CallID:        firstNonEmpty(assistant.ID, derivedID(turnID, "llm", strconv.Itoa(index))),
			StartUnixNano: callStart, EndUnixNano: callEnd, Status: "ok",
			ExtraAttributes: map[string]any{"timing.source": "dcode_turn_slice"},
		}
		if index == len(assistants)-1 {
			call.FinishReasons = []string{"stop"}
		}
		if options.CaptureContent != "none" {
			if index == 0 {
				call.InputMessages = turn.InputMessages
				call.InputPreview = turn.InputPreview
			}
			content := assistant.Content
			text := assistant.Text
			if index == len(assistants)-1 && output != "" {
				content = output
				text = output
			}
			call.OutputMessages = assistantMessage(content, options.MaxChars)
			call.OutputPreview = preview.Text(text, options.MaxChars)
			call.OutputKind = "text"
			if strings.TrimSpace(text) == "" && len(tools) > 0 {
				call.OutputKind = "tool_call"
			}
		}
		turn.LLMCalls = append(turn.LLMCalls, call)
	}

	triggeringCalls := toolTriggeringCalls(assistants)
	for index, raw := range tools {
		toolStart, toolEnd, timingSource := toolWindow(raw, start, end)
		arguments := firstNonNil(raw.Pre.Payload["tool_input"], raw.Post.Payload["tool_input"])
		result := raw.Post.Payload["tool_response"]
		tool := model.ToolCall{
			CallID: raw.ID, Name: firstNonEmpty(raw.Name, eventToolName(raw.Pre.Payload), eventToolName(raw.Post.Payload), "unknown"),
			StartUnixNano: toolStart, EndUnixNano: toolEnd, Status: "ok", ResultStatus: "completed",
			ExtraAttributes: map[string]any{"timing.source": timingSource},
		}
		if index < len(triggeringCalls) {
			tool.TriggeringLLMCall = triggeringCalls[index]
		}
		if raw.Failure {
			tool.Status = "error"
			tool.ResultStatus = "error"
			tool.ErrorType = "tool_error"
			tool.Reason = stringValue(raw.Post.Payload, "error")
			result = firstNonNil(raw.Post.Payload["error"], result)
		}
		if raw.Interrupted {
			tool.ResultStatus = "cancelled"
			tool.ErrorType = "cancelled"
			tool.ExtraAttributes["is_interrupt"] = true
			if tool.Reason == "" {
				tool.Reason = "Tool use interrupted"
			}
			turn.FinalStatus = model.FinalStatusCancelled
		}
		if raw.AgentID != "" {
			tool.ExtraAttributes["gen_ai.subagent.id"] = raw.AgentID
			if raw.AgentName != "" {
				tool.ExtraAttributes["gen_ai.subagent.name"] = raw.AgentName
			}
		}
		if options.CaptureContent != "none" {
			tool.Arguments = privacy.Sanitize(arguments, options.MaxChars)
			tool.Result = privacy.Sanitize(result, options.MaxChars)
			tool.Command = commandValue(arguments, options.MaxChars)
			tool.InputPreview = preview.Text(arguments, options.MaxChars)
			tool.OutputPreview = preview.Text(result, options.MaxChars)
		}
		turn.ToolCalls = append(turn.ToolCalls, tool)
	}

	if output != "" {
		assistantStart := end - 1
		if assistantStart < start {
			assistantStart = start
		}
		assistant := model.AssistantOutput{
			StartUnixNano: assistantStart, EndUnixNano: end, OutputKind: "text", Status: "ok",
			ExtraAttributes: map[string]any{"timing.source": "dcode_stop"},
		}
		if options.CaptureContent != "none" {
			assistant.OutputMessages = turn.OutputMessages
			assistant.OutputPreview = turn.OutputPreview
		}
		turn.AssistantOutputs = append(turn.AssistantOutputs, assistant)
	}
	return turn
}

func failedSessionEnd(events []JournalEvent) (JournalEvent, bool) {
	if latestEventTime(events, "Stop") > 0 {
		return JournalEvent{}, false
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if strings.EqualFold(event.Event, "SessionEnd") && strings.EqualFold(stringValue(event.Payload, "reason"), "other") {
			return event, true
		}
	}
	return JournalEvent{}, false
}

func toolTriggeringCalls(assistants []rawAssistant) []string {
	var out []string
	for _, assistant := range assistants {
		for index := 0; index < assistant.ToolCount; index++ {
			out = append(out, assistant.ID)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, assistant := range assistants {
		if strings.TrimSpace(assistant.Text) == "" {
			out = append(out, assistant.ID)
		}
	}
	return out
}

func toolWindow(value toolBoundary, parentStart, parentEnd int64) (int64, int64, string) {
	start := value.Pre.RecordedNano
	end := value.Post.RecordedNano
	duration := int64Value(value.Post.Payload["duration_ms"])
	source := "dcode_hooks"
	if duration > 0 && end > 0 {
		start = end - duration*int64(time.Millisecond)
		source = "dcode_duration_ms"
	}
	if start <= 0 {
		start = parentStart
	}
	if end <= 0 {
		end = start + 1
	}
	if start < parentStart {
		start = parentStart
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
	return start, end, source
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

func contentText(value any) string {
	var parts []string
	collectText(value, &parts)
	return strings.TrimSpace(strings.Join(parts, ""))
}

func collectText(value any, parts *[]string) {
	switch current := value.(type) {
	case string:
		*parts = append(*parts, current)
	case []any:
		for _, item := range current {
			collectText(item, parts)
		}
	case map[string]any:
		for _, key := range []string{"text", "content"} {
			if item := current[key]; item != nil {
				collectText(item, parts)
				return
			}
		}
	}
}

func equivalentText(left, right string) bool {
	return strings.TrimSpace(left) == strings.TrimSpace(right)
}

func assistantMessage(content any, maxChars int) any {
	text := contentText(content)
	if text == "" {
		return nil
	}
	return textMessage("assistant", text, maxChars)
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
	return stringValue(value, "tool_use_id", "toolUseId", "tool_call_id", "toolCallId", "id")
}

func eventToolName(value map[string]any) string {
	return stringValue(value, "tool_name", "toolName", "name")
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

func stringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
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

func derivedID(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:12])
}
