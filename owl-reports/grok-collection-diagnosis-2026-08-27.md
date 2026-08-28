# Grok collection diagnosis — 2026-08-27

- Time range: 2026-08-27 16:00:00–16:35:00 UTC+08:00
- Data domain: Guance Agent Monitoring (`AM::agent_trace`)
- Tools: `owl.data.show_dql_namespace`, `owl.data.check_dql`, `owl.data.query`

## Conclusion

Two independent issues were confirmed. Queue starvation originally delayed the upload and has been fixed. The uploaded multi-call trace contains the exact turn-level token aggregate on `invoke_agent`, but the Agent Monitoring summary still renders `-` because the two `llm` spans have no provable per-call token split. The selected LLM spans also lacked content because the first implementation did not replay Grok's separate `chat_history.jsonl` conversation file.

## Evidence

- Local Hook log recorded events through 16:07:16, but no successful upload after 16:03:21 until the newest queue was processed manually.
- The queued turn parsed successfully with two LLM calls, three tool calls, 42,528 input tokens, and 1,176 output tokens.
- Manual processing completed at 16:19:37 and recorded successful Trace and Metrics uploads.
- Guance query returned trace `97168b67e73d146ee9151bab18a5c191` with both canonical and compatibility token fields:
  - `gen_ai_usage_input_tokens=42528`
  - `gen_ai_usage_output_tokens=1176`
  - `usage_input_tokens=42528`
  - `usage_output_tokens=1176`
- Input and output previews are present on the uploaded trace.
- Grok's local `chat_history.jsonl` contains two assistant responses for this prompt: the first emits three `web_search` tool calls, and the second emits the final answer. Its persisted assistant records contain no usage field.

## Inference

The missing Trace was caused by local queue starvation, not endpoint authentication, Hook registration, transcript parsing, or backend rejection. The token summary is now a view-compatibility gap rather than missing connector data: displaying the aggregate on either child would misattribute one turn's total to one call, while copying it to both would double count. The missing LLM input/output was a connector parsing gap and can be corrected from prompt-indexed chat history without inventing data.

## Remediation

- Prefer recently modified queue entries when starting bounded detached workers.
- Skip completed turns during transcript recovery.
- Remove an already-completed queue before attempting transcript parsing.
- Added race-tested regression coverage for queue ordering and completed-queue cleanup.
- Correlate the turn's `promptIndex` to `chat_history.jsonl`, require an exact assistant/call-count match, and attach sanitized incremental inputs and outputs to each LLM span.
- Preserve multi-call tokens only on the root until Grok exposes per-call usage; update the Agent Monitoring waterfall to fall back to root aggregate usage when child usage is unavailable.
