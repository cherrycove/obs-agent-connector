package parse

import "strings"

// PromptOrigin is the connector's telemetry policy for a Grok prompt ID.
// The prefixes intentionally mirror Grok Build's PromptOrigin::from_prompt_id
// rules. They are case-sensitive and only match at the beginning of the ID.
type PromptOrigin struct {
	Name        string
	RequestType string
	Suppressed  bool
}

// ClassifyPromptID distinguishes human prompts from Grok runtime-generated
// turns without inspecting prompt text or relying on timing proximity.
func ClassifyPromptID(promptID string) PromptOrigin {
	cases := []struct {
		prefix      string
		name        string
		requestType string
		suppressed  bool
	}{
		{prefix: "task-completed-", name: "task_completed", requestType: "internal", suppressed: true},
		{prefix: "subagent-completed-", name: "subagent_completed", requestType: "internal", suppressed: true},
		{prefix: "parent-message-", name: "parent_agent_message", requestType: "subagent"},
		{prefix: "workflow-completed-", name: "workflow_completed", requestType: "internal", suppressed: true},
		{prefix: "notifications-", name: "notification_drain", requestType: "internal", suppressed: true},
		{prefix: "goal-summary-", name: "goal_summary", requestType: "internal", suppressed: true},
		{prefix: "goal-classifier-nudge-", name: "goal_classifier_nudge", requestType: "internal", suppressed: true},
		{prefix: "scheduler-fired-", name: "scheduler_fired", requestType: "scheduled_task"},
		{prefix: "plan-resume-", name: "plan_resume", requestType: "plan_resume"},
	}
	for _, candidate := range cases {
		if strings.HasPrefix(promptID, candidate.prefix) {
			return PromptOrigin{
				Name: candidate.name, RequestType: candidate.requestType, Suppressed: candidate.suppressed,
			}
		}
	}
	return PromptOrigin{Name: "user", RequestType: "user_request"}
}
