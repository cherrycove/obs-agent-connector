package parse

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/model"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/preview"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/privacy"
)

type chatHistoryItem struct {
	Type        string                `json:"type"`
	Content     any                   `json:"content"`
	PromptIndex *int64                `json:"prompt_index"`
	ModelID     string                `json:"model_id"`
	ToolCalls   []chatHistoryToolCall `json:"tool_calls"`
	ToolCallID  string                `json:"tool_call_id"`
}

type chatHistoryToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

type chatHistoryCall struct {
	ModelID                 string
	InputMessages           any
	OutputMessages          any
	InputPreview            string
	OutputPreview           string
	OutputKind              string
	ToolCallIDs             []string
	AssistantOutputMessages any
	AssistantOutputPreview  string
}

// enrichCallsFromChatHistory maps the persisted conversation for one prompt to
// its already-proven LLM boundaries. chat_history.jsonl is used for content and
// model identity only; it does not contain per-call usage in Grok Build 1.0.5.
func enrichCallsFromChatHistory(turn *model.Turn, transcriptPath string, terminal transcriptRecord, maxChars int) {
	if turn == nil || len(turn.LLMCalls) == 0 {
		return
	}
	promptIndex, ok, err := promptIndexBeforeTerminal(transcriptPath, terminal)
	if err != nil || !ok {
		return
	}
	items, err := readChatHistoryPrompt(filepath.Join(filepath.Dir(transcriptPath), "chat_history.jsonl"), promptIndex)
	if err != nil {
		return
	}
	calls := buildChatHistoryCalls(items, maxChars)
	if len(calls) != len(turn.LLMCalls) {
		return
	}

	triggerByToolID := make(map[string]string)
	for index := range turn.LLMCalls {
		call := &turn.LLMCalls[index]
		content := calls[index]
		call.InputMessages = content.InputMessages
		call.OutputMessages = content.OutputMessages
		call.InputPreview = content.InputPreview
		call.OutputPreview = content.OutputPreview
		call.OutputKind = content.OutputKind
		if call.RequestModel == "" {
			call.RequestModel = content.ModelID
		}
		if call.ResponseModel == "" {
			call.ResponseModel = content.ModelID
		}
		if call.ExtraAttributes == nil {
			call.ExtraAttributes = map[string]any{}
		}
		call.ExtraAttributes["content.source"] = "grok_chat_history"
		for _, toolCallID := range content.ToolCallIDs {
			triggerByToolID[toolCallID] = call.CallID
		}
	}
	for index := range turn.ToolCalls {
		if callID := triggerByToolID[turn.ToolCalls[index].CallID]; callID != "" {
			turn.ToolCalls[index].TriggeringLLMCall = callID
		}
	}
	enrichAssistantOutputs(turn, calls)
}

// enrichAssistantOutputs emits a local assistant event only when the persisted
// response contains visible text. Tool-call-only responses remain represented
// by their LLM and tool spans. The terminal assistant created from
// TurnCompleted is enriched with the last persisted response instead of being
// duplicated.
func enrichAssistantOutputs(turn *model.Turn, calls []chatHistoryCall) {
	type persistedAssistant struct {
		CallIndex int
		Output    model.AssistantOutput
	}
	persisted := make([]persistedAssistant, 0, len(calls))
	for index, content := range calls {
		if content.AssistantOutputMessages == nil || strings.TrimSpace(content.AssistantOutputPreview) == "" {
			continue
		}
		call := turn.LLMCalls[index]
		start, end := assistantWindow(call.EndUnixNano, turn.StartUnixNano, turn.EndUnixNano)
		status := call.Status
		if status == "" {
			status = "ok"
		}
		persisted = append(persisted, persistedAssistant{CallIndex: index, Output: model.AssistantOutput{
			StartUnixNano:  start,
			EndUnixNano:    end,
			OutputMessages: content.AssistantOutputMessages,
			OutputPreview:  content.AssistantOutputPreview,
			OutputKind:     "text",
			Provider:       call.Provider,
			RequestModel:   call.RequestModel,
			ResponseModel:  call.ResponseModel,
			Status:         status,
			ErrorType:      call.ErrorType,
			Reason:         call.Reason,
			ExtraAttributes: map[string]any{
				"content.source": "grok_chat_history",
				"timing.source":  "grok_llm_boundary",
			},
		}})
	}
	if len(persisted) == 0 {
		return
	}

	if len(turn.AssistantOutputs) > 0 {
		lastOutput := len(persisted) - 1
		terminal := len(turn.AssistantOutputs) - 1
		candidate := persisted[lastOutput]
		terminalOutput := turn.AssistantOutputs[terminal]
		if candidate.CallIndex == len(calls)-1 || sameAssistantPreview(candidate.Output, terminalOutput) {
			turn.AssistantOutputs[terminal] = mergeAssistantOutput(terminalOutput, candidate.Output)
			persisted = persisted[:lastOutput]
		}
	}
	outputs := make([]model.AssistantOutput, 0, len(persisted)+len(turn.AssistantOutputs))
	for _, candidate := range persisted {
		outputs = append(outputs, candidate.Output)
	}
	turn.AssistantOutputs = append(outputs, turn.AssistantOutputs...)
}

func sameAssistantPreview(left, right model.AssistantOutput) bool {
	return strings.TrimSpace(left.OutputPreview) != "" && strings.TrimSpace(left.OutputPreview) == strings.TrimSpace(right.OutputPreview)
}

func mergeAssistantOutput(terminal, persisted model.AssistantOutput) model.AssistantOutput {
	terminal.StartUnixNano = persisted.StartUnixNano
	terminal.EndUnixNano = persisted.EndUnixNano
	terminal.OutputMessages = persisted.OutputMessages
	terminal.OutputPreview = persisted.OutputPreview
	terminal.OutputKind = persisted.OutputKind
	terminal.Provider = firstNonEmpty(persisted.Provider, terminal.Provider)
	terminal.RequestModel = firstNonEmpty(persisted.RequestModel, terminal.RequestModel)
	terminal.ResponseModel = firstNonEmpty(persisted.ResponseModel, terminal.ResponseModel)
	if terminal.Status == "" {
		terminal.Status = persisted.Status
	}
	if terminal.ExtraAttributes == nil {
		terminal.ExtraAttributes = map[string]any{}
	}
	for key, value := range persisted.ExtraAttributes {
		terminal.ExtraAttributes[key] = value
	}
	return terminal
}

func promptIndexBeforeTerminal(path string, terminal transcriptRecord) (int64, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()

	var candidate int64
	found := false
	line := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if line >= terminal.Index {
			break
		}
		line++
		var record transcriptRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if record.Params.SessionID != "" && terminal.Params.SessionID != "" && record.Params.SessionID != terminal.Params.SessionID {
			continue
		}
		if strings.EqualFold(record.Method, "_x.ai/session/update") && updateType(record) == "turn_completed" {
			found = false
			continue
		}
		if !strings.EqualFold(record.Method, "session/update") || updateType(record) != "user_message_chunk" {
			continue
		}
		meta, _ := record.Params.Update["_meta"].(map[string]any)
		value, exists := firstMapValue(meta, "promptIndex", "prompt_index")
		if !exists {
			continue
		}
		candidate = int64Value(value)
		found = true
	}
	if err := scanner.Err(); err != nil {
		return 0, false, err
	}
	return candidate, found, nil
}

func readChatHistoryPrompt(path string, promptIndex int64) ([]chatHistoryItem, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	items := make([]chatHistoryItem, 0)
	found := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var item chatHistoryItem
		if json.Unmarshal(scanner.Bytes(), &item) != nil {
			continue
		}
		item.Type = strings.ToLower(strings.TrimSpace(item.Type))
		if item.Type == "user" && item.PromptIndex != nil {
			if found {
				break
			}
			if *item.PromptIndex != promptIndex {
				continue
			}
			found = true
		}
		if found {
			items = append(items, item)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func buildChatHistoryCalls(items []chatHistoryItem, maxChars int) []chatHistoryCall {
	inputs := make([]any, 0)
	calls := make([]chatHistoryCall, 0)
	for _, item := range items {
		switch item.Type {
		case "user":
			if message := chatUserMessage(item.Content, maxChars); message != nil {
				inputs = append(inputs, message)
			}
		case "tool_result":
			if message := chatToolResultMessage(item, maxChars); message != nil {
				inputs = append(inputs, message)
			}
		case "assistant":
			output, assistantOutput, assistantPreview, toolIDs := chatAssistantMessage(item, maxChars)
			if output == nil {
				continue
			}
			inputMessages := nilIfEmptyMessages(inputs)
			outputMessages := []any{output}
			kind := "text"
			if len(toolIDs) > 0 {
				kind = "tool_call"
			}
			calls = append(calls, chatHistoryCall{
				ModelID: item.ModelID, InputMessages: inputMessages, OutputMessages: outputMessages,
				InputPreview: messagePreview(inputMessages, maxChars), OutputPreview: messagePreview(outputMessages, maxChars),
				OutputKind: kind, ToolCallIDs: toolIDs,
				AssistantOutputMessages: assistantOutput, AssistantOutputPreview: assistantPreview,
			})
			inputs = nil
		}
	}
	return calls
}

func chatUserMessage(content any, maxChars int) any {
	parts := chatTextParts(content, maxChars)
	if len(parts) == 0 {
		return nil
	}
	return map[string]any{"role": "user", "parts": parts}
}

func chatToolResultMessage(item chatHistoryItem, maxChars int) any {
	response := privacy.Sanitize(item.Content, maxChars)
	if response == nil || strings.TrimSpace(preview.Text(response, maxChars)) == "" {
		return nil
	}
	part := map[string]any{"type": "tool_call_response", "response": response}
	if strings.TrimSpace(item.ToolCallID) != "" {
		part["id"] = item.ToolCallID
	}
	return map[string]any{"role": "tool", "parts": []any{part}}
}

func chatAssistantMessage(item chatHistoryItem, maxChars int) (any, any, string, []string) {
	textParts := chatTextParts(item.Content, maxChars)
	parts := append([]any(nil), textParts...)
	toolIDs := make([]string, 0, len(item.ToolCalls))
	for _, toolCall := range item.ToolCalls {
		if strings.TrimSpace(toolCall.Name) == "" {
			continue
		}
		part := map[string]any{"type": "tool_call", "name": toolCall.Name}
		if strings.TrimSpace(toolCall.ID) != "" {
			part["id"] = toolCall.ID
			toolIDs = append(toolIDs, toolCall.ID)
		}
		if arguments := sanitizedJSONValue(toolCall.Arguments, maxChars); arguments != nil {
			part["arguments"] = arguments
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, nil, "", nil
	}
	finishReason := "stop"
	if len(toolIDs) > 0 {
		finishReason = "tool_call"
	}
	message := map[string]any{"role": "assistant", "parts": parts}
	message["finish_reason"] = finishReason

	var assistantOutput any
	assistantPreview := ""
	if len(textParts) > 0 {
		assistantOutput = []any{map[string]any{
			"role":          "assistant",
			"parts":         textParts,
			"finish_reason": finishReason,
		}}
		assistantPreview = chatTextPreview(textParts, maxChars)
	}
	return message, assistantOutput, assistantPreview, toolIDs
}

func chatTextPreview(parts []any, maxChars int) string {
	texts := make([]string, 0, len(parts))
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		if text, ok := part["content"].(string); ok && strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return preview.Text(strings.Join(texts, "\n"), maxChars)
}

func chatTextParts(content any, maxChars int) []any {
	parts := make([]any, 0)
	appendText := func(value any) {
		text := privacy.Text(value, maxChars)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, map[string]any{"type": "text", "content": text})
		}
	}
	switch current := content.(type) {
	case string:
		appendText(current)
	case []any:
		for _, rawPart := range current {
			part, _ := rawPart.(map[string]any)
			if !strings.EqualFold(stringValue(part, "type"), "text") {
				continue
			}
			if value, ok := firstMapValue(part, "text", "content"); ok {
				appendText(value)
			}
		}
	default:
		if content != nil {
			appendText(content)
		}
	}
	return parts
}

func sanitizedJSONValue(value any, maxChars int) any {
	text, ok := value.(string)
	if !ok || !json.Valid([]byte(text)) {
		return privacy.Sanitize(value, maxChars)
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return privacy.Sanitize(value, maxChars)
	}
	return privacy.Sanitize(decoded, maxChars)
}

func messagePreview(messages any, maxChars int) string {
	return preview.Text(messages, maxChars)
}

func nilIfEmptyMessages(messages []any) any {
	if len(messages) == 0 {
		return nil
	}
	return messages
}

func firstMapValue(value map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if current, ok := value[key]; ok {
			return current, true
		}
	}
	return nil, false
}
