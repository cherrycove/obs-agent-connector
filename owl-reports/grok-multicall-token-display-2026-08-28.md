# Grok multi-call token display diagnosis — 2026-08-28

## Scope

- Absolute time range: 2026-08-28 11:26:00–11:29:00 UTC+08:00, with a six-hour comparison window ending at the diagnostic time.
- Trace: `b0fa06562605eedd3b945c9ccf3609e7`.
- Data domains: Guance Agent Monitoring (`AM::agent_trace`), local Grok `updates.jsonl`, `events.jsonl`, and `chat_history.jsonl`.
- Tools: `owl.data.show_dql_namespace`, `owl.data.check_dql`, `owl.data.query`, local structured JSON projections, and the pinned Grok Build 1.0.5 source.

## Overall conclusion

Token usage was collected and received. The `invoke_agent` root contains the exact turn aggregate: 109,614 input tokens, 1,950 output tokens, 81,856 cached-read input tokens, and 1,083 reasoning output tokens. The trace contains four real `llm` spans, but none has attributable per-call usage. The Trace list therefore shows `-` because its current token summary does not fall back to the root aggregate when child LLM usage is absent.

This is not caused by the hidden runtime-turn suppression repair. It is the existing multi-call display limitation: single-call Grok turns can safely copy the one-call aggregate to their only LLM span, while multi-call turns cannot.

## Evidence

- `AM::agent_trace` contains the exact canonical `gen_ai_usage_input_tokens` and `gen_ai_usage_output_tokens`, the root-only compatibility aliases, and `gtrace_usage` at the root timestamp.
- The same trace contains four event-derived `llm` spans without usage attributes.
- The matching Grok `turn_completed` update reports `modelCalls=4` and only the aggregate usage above.
- Grok `events.jsonl` provides the four real call boundaries but no usage values.
- Persisted assistant items in `chat_history.jsonl` contain model/content metadata but no usage field.
- Earlier one-call Grok traces show tokens because `modelCalls=1` makes the turn aggregate attributable to the only LLM call.

## Inference

The Agent Monitoring list/detail summary is deriving displayed token totals from child LLM usage, or otherwise requiring child usage before rendering the summary. It is not consuming the already-present root aggregate for this multi-call case.

## Implemented connector compatibility

At the user's request, the connector now exposes estimated per-call token fields when exact per-response usage is unavailable. The compatibility behavior uses this precedence:

1. Keep exact `ResponseCompleted` usage unchanged.
2. Require complete aggregate usage and an exact `modelCalls`/LLM-call-count match.
3. Apportion input/cache tokens by relative call input size and output/reasoning tokens by relative call output size, with call duration as the no-content fallback.
4. Preserve every aggregate category exactly by largest-remainder allocation.
5. Mark every apportioned call with `gtrace.usage.estimated=true` and `gtrace.usage.source=grok_turn_completed_proportional`.

The connector does not copy the full turn aggregate to the final LLM or to every LLM. The allocated per-call values are estimates, while their sum and the root values remain exact.

## Remaining gap

Grok still does not expose exact per-response usage for this backend. A future product record should replace the estimated allocation automatically; the connector already gives exact `ResponseCompleted` usage precedence when it is available.
