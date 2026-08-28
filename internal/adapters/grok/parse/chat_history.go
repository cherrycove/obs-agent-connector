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
	ModelID        string
	InputMessages  any
	OutputMessages any
	InputPreview   string
	OutputPreview  string
	OutputKind     string
	ToolCallIDs    []string
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
			output, toolIDs := chatAssistantMessage(item, maxChars)
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

func chatAssistantMessage(item chatHistoryItem, maxChars int) (any, []string) {
	parts := chatTextParts(item.Content, maxChars)
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
		return nil, nil
	}
	message := map[string]any{"role": "assistant", "parts": parts}
	if len(toolIDs) > 0 {
		message["finish_reason"] = "tool_call"
	} else {
		message["finish_reason"] = "stop"
	}
	return message, toolIDs
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
