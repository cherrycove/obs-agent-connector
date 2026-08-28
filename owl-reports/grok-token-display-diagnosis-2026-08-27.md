# Grok token display diagnosis — 2026-08-27

- Time range: 2026-08-27 17:00:00–17:20:00 UTC+08:00
- Trace: `852ff63c65a03b46485ab5371c7e2db0`
- Data domains: Guance Agent Monitoring (`AM::agent_trace`) and Metrics (`M::agent_runtime`)
- Tools: `owl.data.show_dql_namespace`, `owl.data.check_dql`, `owl.data.query`, `owl.metric.list`

## Overall conclusion

The input/output token aggregate was received successfully on the `invoke_agent` root span, but the built-in Grok adapter omitted the `gtrace.usage` compatibility payload used by the current Agent Monitoring observation summary. The connector now emits both the canonical `gen_ai.usage.*` attributes and `gtrace.usage` on the root; LLM spans receive the compatibility payload only when Grok provides usage attributable to that single call. No `gen_ai.client.token.usage` metric exists for this multi-call turn because the connector intentionally derives token metrics only from per-call `llm` usage; it is not a Metrics receiver failure.

## Evidence

- The root span contains:
  - `gen_ai_usage_input_tokens=70802`
  - `gen_ai_usage_output_tokens=1245`
  - `gen_ai_usage_cache_read_input_tokens=22208`
  - `gen_ai_usage_reasoning_output_tokens=451`
  - compatibility aliases `usage_input_tokens=70802` and `usage_output_tokens=1245`
- The trace contains three `llm` spans. Their input/output content is present, but none has `gen_ai_usage_input_tokens` or `gen_ai_usage_output_tokens` because Grok supplied only one turn aggregate for three model calls.
- Metrics for the same time window and `agent_runtime=grok` were received:
  - `workflow_count=1`
  - `operation_count=8`, matching three LLM calls and five tool calls
- The Metrics field catalog contains workflow and operation fields but no `gen_ai_client_token_usage` field for this data, and a direct token-usage query returned an empty result.
- The final `assistant` span contains the delivered output and has no token fields, as required by the Agent semantic baseline.

## Inferences

- The `-` values in the trace summary are not evidence that OTLP trace ingestion lost the turn totals; the root values prove successful trace ingestion. Comparison with the Claude implementation showed that the missing GTrace compatibility payload prevented the current observation summary from consuming those totals.
- Because other metrics from the same upload were stored, the missing token metric is not explained by endpoint authentication, transport failure, or whole-batch rejection.
- Assigning the turn aggregate to one child LLM would misattribute usage; assigning it to all three would triple count it.

## Next steps and gaps

1. Deploy the connector repair and validate a newly generated Grok turn. Existing stored traces are immutable and will not be backfilled.
2. Keep `assistant` excluded from token and operation metrics.
3. For per-LLM token badges and token metric charts, wait for Grok to expose per-call usage or define a separate turn-aggregate token metric contract. Do not reuse the per-call `gen_ai.client.token.usage` metric without an explicit semantic decision.
4. If the repaired root summary is still not consumed, update the Agent Monitoring view to fall back from child LLM usage to the root aggregate. A current frontend source repository was not available in this workspace.

## Comparative adapter audit

- Audit time: 2026-08-27 17:35–17:55 UTC+08:00.
- The comparison Trace `d10c99bcc442b8acab6e03e466833942` belongs to another workspace and returned no rows through the current Owl workspace. Its screenshot was therefore treated as visual evidence, not queried raw data.
- Codex constructs one LLM span per step from `last_token_usage` and emits an assistant span for every non-empty assistant message in that step.
- Claude persists usage on each assistant response, maps that response to one LLM call, and emits an assistant span only when the response contains text.
- WorkBuddy likewise parses per-message/provider usage and emits assistant spans from non-empty assistant messages.
- Grok Build's persisted `AssistantItem` contains content, tool calls, and model identity but not usage. The runtime `ConversationResponse` has usage before persistence, while `TurnCompleted` folds multiple calls into one aggregate.
- The inspected Grok prompt has three persisted assistant responses: `(text=0, tools=3)`, `(text=0, tools=2)`, and `(text=699, tools=0)`. One assistant span is therefore correct for this prompt. Empty tool-call-only responses remain represented by their LLM output messages and tool spans.
- The repair now creates an assistant span for every persisted non-empty Grok assistant response and merges the final persisted response with the terminal assistant. Tool-call-only responses do not create empty assistant spans.

## Connector repair

- Added `gtrace.observation.type`, `gtrace.observation.input`, `gtrace.observation.output`, and `gtrace.model.name` compatibility attributes from the same sanitized, bounded values used by the canonical spans.
- Added JSON `gtrace.usage` on the root aggregate and on LLM spans only when their single-call usage is known.
- Added intermediate visible assistant mapping, final assistant deduplication, and regressions for tool-call-only and unmatched terminal output cases.
- Preserved the rule that turn aggregate usage is never divided, copied, or assigned to multiple LLM calls.
