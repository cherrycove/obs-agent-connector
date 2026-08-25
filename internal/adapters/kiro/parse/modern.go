package parse

import (
	"bufio"
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

type modernSession struct {
	Metadata     map[string]any
	ID           string
	MessagesPath string
}

type modernAssistant struct {
	Text         string
	RecordedNano int64
}

type modernTool struct {
	ID           string
	Name         string
	Arguments    any
	Result       any
	Status       string
	Success      *bool
	StartNano    int64
	EndNano      int64
	DurationNano int64
}

type modernTurn struct {
	Prompt      string
	ExecutionID string
	StartNano   int64
	EndNano     int64
	Credit      float64
	StopReason  string
	UsageStatus string
	Terminal    bool
	Assistants  []modernAssistant
	Tools       []modernTool
	RequestIDs  []string
}

func findModernSession(sessionDir, sessionID string) (modernSession, bool, error) {
	if strings.TrimSpace(sessionDir) == "" {
		return modernSession{}, false, errors.New("Kiro session directory is empty")
	}
	if !safeID(sessionID) {
		return modernSession{}, false, nil
	}
	seen := map[string]bool{}
	for _, root := range modernSessionRoots(sessionDir) {
		candidates := []string{root, filepath.Join(root, sessionID)}
		entries, err := os.ReadDir(root)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					candidates = append(candidates, filepath.Join(root, entry.Name(), sessionID))
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return modernSession{}, false, err
		}
		for _, directory := range candidates {
			directory = filepath.Clean(directory)
			if seen[directory] {
				continue
			}
			seen[directory] = true
			metadata, err := readObject(filepath.Join(directory, "session.json"))
			if err != nil || stringValue(metadata, "id") != sessionID {
				continue
			}
			messagesPath := filepath.Join(directory, "messages.jsonl")
			if _, err := os.Stat(messagesPath); err != nil {
				continue
			}
			return modernSession{Metadata: metadata, ID: sessionID, MessagesPath: messagesPath}, true, nil
		}
	}
	return modernSession{}, false, nil
}

func modernSessionRoots(sessionDir string) []string {
	clean := filepath.Clean(sessionDir)
	if filepath.Base(clean) == "cli" {
		return []string{filepath.Dir(clean)}
	}
	return []string{clean}
}

func readModernTurn(session modernSession, options Options) (model.Turn, bool, error) {
	turns, err := readModernTurns(session.MessagesPath)
	if err != nil {
		return model.Turn{}, false, err
	}
	expectedPrompt := latestEventString(options.Events, "UserPromptSubmit", "prompt")
	promptTime := latestEventTime(options.Events, "UserPromptSubmit")
	stopTime := latestEventTime(options.Events, "Stop")
	selected := -1
	bestDistance := int64(^uint64(0) >> 1)
	for index := len(turns) - 1; index >= 0; index-- {
		current := turns[index]
		if !current.Terminal {
			continue
		}
		if promptTime > 0 {
			if stopTime > 0 && current.EndNano > stopTime+int64(2*time.Second) {
				continue
			}
			distance := absoluteDistance(current.StartNano, promptTime)
			if distance < bestDistance {
				selected = index
				bestDistance = distance
			}
			continue
		}
		if expectedPrompt == "" || strings.TrimSpace(current.Prompt) == strings.TrimSpace(expectedPrompt) {
			selected = index
			break
		}
	}
	if selected < 0 {
		return model.Turn{}, false, nil
	}
	turn := normalizeModern(session, turns[selected], options)
	if turn.SessionID == "" || turn.TurnID == "" || turn.FinalStatus == model.FinalStatusUnset {
		return model.Turn{}, false, nil
	}
	return turn, true, nil
}

func readModernTurns(path string) ([]modernTurn, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	turns := make([]modernTurn, 0)
	var current *modernTurn
	toolIndexes := map[string]int{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		payload := object(record["payload"])
		recordType := stringValue(payload, "type")
		recordedNano := parseTime(record["timestamp"])
		switch recordType {
		case "user":
			if current != nil && current.Terminal {
				turns = append(turns, *current)
			}
			current = &modernTurn{Prompt: scalarText(payload["content"]), StartNano: recordedNano}
			toolIndexes = map[string]int{}
		case "turn_start":
			if current == nil {
				current = &modernTurn{StartNano: recordedNano}
				toolIndexes = map[string]int{}
			}
			current.ExecutionID = firstNonEmpty(current.ExecutionID, stringValue(payload, "executionId"))
			if current.StartNano == 0 {
				current.StartNano = recordedNano
			}
		case "assistant":
			if !belongsToModernTurn(current, payload) {
				continue
			}
			if stringValue(payload, "subExecutionId") != "" {
				continue
			}
			operationType := stringValue(payload, "operationType")
			if modernVisibleAssistant(operationType) {
				current.Assistants = append(current.Assistants, modernAssistant{
					Text: scalarText(payload["content"]), RecordedNano: recordedNano,
				})
			}
		case "tool_call":
			if !belongsToModernTurn(current, payload) {
				continue
			}
			if stringValue(payload, "subExecutionId") != "" {
				continue
			}
			id := stringValue(payload, "toolCallId")
			index, ok := toolIndexes[id]
			if !ok || id == "" {
				current.Tools = append(current.Tools, modernTool{ID: id})
				index = len(current.Tools) - 1
				if id != "" {
					toolIndexes[id] = index
				}
			}
			tool := &current.Tools[index]
			tool.Name = firstNonEmpty(tool.Name, normalizedToolName(stringValue(payload, "toolName")))
			if tool.Arguments == nil {
				tool.Arguments = payload["args"]
			}
			tool.Status = firstNonEmpty(stringValue(payload, "status"), tool.Status)
			if tool.StartNano == 0 || (recordedNano > 0 && recordedNano < tool.StartNano) {
				tool.StartNano = recordedNano
			}
		case "tool_result":
			if !belongsToModernTurn(current, payload) {
				continue
			}
			if stringValue(payload, "subExecutionId") != "" {
				continue
			}
			id := stringValue(payload, "toolCallId")
			index, ok := toolIndexes[id]
			if !ok {
				current.Tools = append(current.Tools, modernTool{ID: id})
				index = len(current.Tools) - 1
				if id != "" {
					toolIndexes[id] = index
				}
			}
			tool := &current.Tools[index]
			tool.Result = payload["content"]
			tool.EndNano = recordedNano
			tool.DurationNano = int64Value(payload["durationMs"]) * int64(time.Millisecond)
			if success, ok := payload["success"].(bool); ok {
				tool.Success = &success
			}
		case "usage_summary":
			if !belongsToModernTurn(current, payload) {
				continue
			}
			current.Credit = modernCreditUsage(payload["promptTurnSummaries"])
			current.UsageStatus = stringValue(payload, "status")
			current.RequestIDs = stringSlice(payload["requestIds"])
		case "turn_end":
			if !belongsToModernTurn(current, payload) {
				continue
			}
			current.ExecutionID = firstNonEmpty(current.ExecutionID, stringValue(payload, "executionId"))
			current.StopReason = stringValue(payload, "stopReason")
			current.EndNano = recordedNano
			current.Terminal = true
		}
	}
	if current != nil && current.Terminal {
		turns = append(turns, *current)
	}
	return turns, scanner.Err()
}

func belongsToModernTurn(turn *modernTurn, payload map[string]any) bool {
	if turn == nil || turn.Terminal {
		return false
	}
	executionID := stringValue(payload, "executionId")
	return executionID == "" || turn.ExecutionID == "" || executionID == turn.ExecutionID
}

func modernVisibleAssistant(operationType string) bool {
	switch strings.ToLower(strings.TrimSpace(operationType)) {
	case "", "say", "print", "summary":
		return true
	default:
		return false
	}
}

func normalizeModern(session modernSession, raw modernTurn, options Options) model.Turn {
	start, end := raw.StartNano, raw.EndNano
	if start <= 0 {
		start = latestEventTime(options.Events, "UserPromptSubmit")
	}
	if end <= 0 {
		end = latestEventTime(options.Events, "Stop")
	}
	if start <= 0 || start >= end {
		start = end - int64(time.Millisecond)
	}
	status, errorType, reason := modernFinalStatus(raw.UsageStatus, raw.StopReason)
	output := modernAssistantText(raw.Assistants)
	outputTimingSource := "kiro_message_journal"
	if output == "" {
		output = options.AssistantResponse
		if output != "" {
			outputTimingSource = "kiro_hook"
		}
	}
	modelName := stringValue(session.Metadata, "modelId")
	resource := copyMap(options.ResourceAttributes)
	resource["agent_runtime"] = "kiro"
	resource["telemetry.sdk.language"] = "go"
	resource["telemetry.sdk.name"] = "gtrace"
	turn := model.Turn{
		SessionID: session.ID, TurnID: firstNonEmpty(raw.ExecutionID, derivedID(session.ID, strconv.FormatInt(start, 10), raw.Prompt)),
		AgentRuntime: "kiro", AgentName: "Kiro", AgentVersion: options.AgentVersion,
		StartUnixNano: start, EndUnixNano: end, FinalStatus: status,
		InputLength: len([]rune(raw.Prompt)), OutputLength: len([]rune(output)),
		CreditUsage: raw.Credit, Resource: resource, ErrorType: errorType, Reason: reason,
		ExtraAttributes: map[string]any{"request_type": "user_request", "timing.source": "kiro_message_journal"},
	}
	if options.CaptureContent != "none" {
		turn.InputMessages = textMessage("user", raw.Prompt, options.MaxChars)
		turn.OutputMessages = textMessage("assistant", output, options.MaxChars)
		turn.InputPreview = preview.Text(raw.Prompt, options.MaxChars)
		turn.OutputPreview = preview.Text(output, options.MaxChars)
	}

	requestIDs := append([]string(nil), raw.RequestIDs...)
	if len(requestIDs) == 0 && (len(raw.Assistants) > 0 || len(raw.Tools) > 0) {
		requestIDs = []string{derivedID(turn.TurnID, "llm", "0")}
	}
	assistantTextByCall := distributeModernAssistantText(raw.Assistants, len(requestIDs))
	for index, requestID := range requestIDs {
		callStart, callEnd := sliceWindow(start, end, index, len(requestIDs))
		call := model.LLMCall{
			CallID: requestID, StartUnixNano: callStart, EndUnixNano: callEnd,
			Provider: providerForModel(modelName), RequestModel: modelName, ResponseModel: modelName,
			FinishReasons: []string{firstNonEmpty(raw.StopReason, raw.UsageStatus)},
			Status:        statusValue(errorType), ErrorType: errorType, Reason: reason,
			ExtraAttributes: map[string]any{"timing.source": "kiro_request_order_slice"},
		}
		if options.CaptureContent != "none" {
			if index == 0 {
				call.InputMessages = textMessage("user", raw.Prompt, options.MaxChars)
				call.InputPreview = preview.Text(raw.Prompt, options.MaxChars)
			}
			if assistantTextByCall[index] != "" {
				call.OutputMessages = textMessage("assistant", assistantTextByCall[index], options.MaxChars)
				call.OutputPreview = preview.Text(assistantTextByCall[index], options.MaxChars)
				call.OutputKind = "text"
			}
		}
		turn.LLMCalls = append(turn.LLMCalls, call)
	}

	preEvents, postEvents := toolEvents(options.Events)
	for index, rawTool := range raw.Tools {
		pre, post := matchToolEvent(rawToolForMatch(rawTool), preEvents, postEvents, index)
		toolStart, toolEnd := rawTool.StartNano, rawTool.EndNano
		timingSource := "kiro_message_journal"
		if rawTool.DurationNano > 0 && toolEnd > 0 && (toolStart <= 0 || toolEnd-toolStart < rawTool.DurationNano) {
			toolStart = toolEnd - rawTool.DurationNano
		}
		if pre.RecordedNano > 0 {
			toolStart = pre.RecordedNano
			timingSource = "kiro_hook"
		}
		if post.RecordedNano > toolStart {
			toolEnd = post.RecordedNano
			timingSource = "kiro_hook"
		}
		if toolStart <= 0 {
			toolStart = start
		}
		if toolEnd <= toolStart {
			toolEnd = toolStart + 1
		}
		arguments := firstNonNil(rawTool.Arguments, pre.Payload["tool_input"], pre.Payload["toolInput"])
		result := firstNonNil(rawTool.Result, post.Payload["tool_response"], post.Payload["toolResponse"], post.Payload["tool_output"], post.Payload["toolOutput"])
		toolStatus, resultStatus, toolError := modernToolStatus(rawTool, post)
		triggeringCall := ""
		if len(requestIDs) > 0 {
			triggeringCall = requestIDs[minInt(index, len(requestIDs)-1)]
		}
		tool := model.ToolCall{
			CallID:            firstNonEmpty(rawTool.ID, eventToolID(pre.Payload), derivedID(turn.TurnID, "tool", strconv.Itoa(index))),
			TriggeringLLMCall: triggeringCall, Name: firstNonEmpty(rawTool.Name, eventToolName(pre.Payload), "unknown"),
			StartUnixNano: toolStart, EndUnixNano: toolEnd, Status: toolStatus, ResultStatus: resultStatus, ErrorType: toolError,
			ExtraAttributes: map[string]any{"timing.source": timingSource, "correlation.source": "kiro_request_order"},
		}
		if options.CaptureContent != "none" {
			tool.Arguments = privacy.Sanitize(arguments, options.MaxChars)
			tool.Result = privacy.Sanitize(result, options.MaxChars)
			tool.InputPreview = preview.Text(arguments, options.MaxChars)
			tool.OutputPreview = preview.Text(result, options.MaxChars)
			tool.Command = commandValue(arguments, options.MaxChars)
		}
		turn.ToolCalls = append(turn.ToolCalls, tool)
	}

	if output != "" {
		assistantStart := latestModernAssistantTime(raw.Assistants)
		if assistantStart <= 0 || assistantStart >= end {
			assistantStart = end - 1
		}
		assistantOutput := model.AssistantOutput{
			StartUnixNano: assistantStart, EndUnixNano: end, OutputKind: "text",
			Provider: providerForModel(modelName), RequestModel: modelName, ResponseModel: modelName,
			Status: statusValue(errorType), ErrorType: errorType, Reason: reason,
			ExtraAttributes: map[string]any{"timing.source": outputTimingSource},
		}
		if options.CaptureContent != "none" {
			assistantOutput.OutputMessages = turn.OutputMessages
			assistantOutput.OutputPreview = turn.OutputPreview
		}
		turn.AssistantOutputs = append(turn.AssistantOutputs, assistantOutput)
	}
	return turn
}

func modernFinalStatus(usageStatus, stopReason string) (model.FinalStatus, string, string) {
	reason := firstNonEmpty(stopReason, usageStatus)
	normalized := strings.ToLower(usageStatus + " " + stopReason)
	switch {
	case strings.Contains(normalized, "cancel"), strings.Contains(normalized, "interrupt"), strings.Contains(normalized, "abort"):
		return model.FinalStatusCancelled, "", reason
	case strings.Contains(normalized, "error"), strings.Contains(normalized, "fail"):
		return model.FinalStatusCompleted, "kiro_agent_error", reason
	default:
		return model.FinalStatusCompleted, "", ""
	}
}

func modernToolStatus(tool modernTool, post JournalEvent) (string, string, string) {
	if boolValue(post.Payload["is_error"]) || boolValue(post.Payload["isError"]) || (tool.Success != nil && !*tool.Success) || strings.EqualFold(tool.Status, "failed") {
		return "error", "error", "tool_error"
	}
	if tool.Success != nil || strings.EqualFold(tool.Status, "completed") || post.RecordedNano > 0 {
		return "ok", "completed", ""
	}
	if strings.EqualFold(tool.Status, "denied") {
		return "ok", "denied", ""
	}
	return "unset", "unset", ""
}

func rawToolForMatch(tool modernTool) rawTool {
	return rawTool{ID: tool.ID, Name: tool.Name, Arguments: tool.Arguments}
}

func modernAssistantText(assistants []modernAssistant) string {
	parts := make([]string, 0, len(assistants))
	for _, assistant := range assistants {
		if text := strings.TrimSpace(assistant.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func distributeModernAssistantText(assistants []modernAssistant, count int) []string {
	out := make([]string, count)
	if count == 0 {
		return out
	}
	for index, assistant := range assistants {
		position := minInt(index, count-1)
		text := strings.TrimSpace(assistant.Text)
		if text == "" {
			continue
		}
		if out[position] != "" {
			out[position] += "\n"
		}
		out[position] += text
	}
	return out
}

func latestModernAssistantTime(assistants []modernAssistant) int64 {
	for index := len(assistants) - 1; index >= 0; index-- {
		if assistants[index].RecordedNano > 0 && strings.TrimSpace(assistants[index].Text) != "" {
			return assistants[index].RecordedNano
		}
	}
	return 0
}

func stringSlice(value any) []string {
	out := make([]string, 0)
	for _, item := range anySlice(value) {
		if text := scalarText(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func modernCreditUsage(value any) float64 {
	total := 0.0
	for _, summary := range objectSlice(value) {
		unit := strings.ToLower(firstNonEmpty(
			stringValue(summary, "unit"),
			stringValue(summary, "unitPlural"),
		))
		if unit != "credit" && unit != "credits" {
			continue
		}
		usage, ok := summary["usage"].(float64)
		if ok && usage > 0 {
			total += usage
		}
	}
	return total
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func absoluteDistance(left, right int64) int64 {
	if left > right {
		return left - right
	}
	return right - left
}
