# Grok background completion turn split diagnosis

## Scope

- Session: `01a04639-c6bd-7af2-8fe2-ce0ba30d2161`
- Time range: 2026-08-28 10:35:00–10:45:00 UTC+08:00
- Data domain: Guance `AM::agent_trace`, local Grok session transcript, connector Hook journal, and Grok adapter source
- Platform query: `AM::agent_trace:(*) {session_id="01a04639-c6bd-7af2-8fe2-ce0ba30d2161"} [10m]`

## Conclusion

The two visually similar records are not duplicate uploads of one trace. Grok Build produced two distinct completed turns with different prompt IDs:

1. The real user request, prompt ID `87948462-d002-4c3f-9595-495cbbb9b789`.
2. An internal continuation generated after a background command completed, prompt ID `task-completed-call-8ff87d53-b110-406e-8cec-119a15d6362e-3`.

The connector version under diagnosis treated every Grok `UserPromptSubmit` record as a user request. Consequently, the internal `<system-reminder>` continuation was exported as a second `invoke_agent` trace and appeared to users as though one question was split into two.

This behavior is incorrect for the user-workflow view, although the second LLM call is real and its token consumption is not a duplicated metric.

## Evidence

### Local Grok transcript

`updates.jsonl` contains three completed turns in the session:

- An earlier `hi` request: 18,269 input tokens and 215 output tokens.
- The real search request: 91,554 input tokens and 2,002 output tokens.
- The background-completion continuation: 26,340 input tokens and 345 output tokens.

The continuation's input begins with:

```text
<system-reminder>
Background task "call-8ff87d53-b110-406e-8cec-119a15d6362e-3" completed ...
</system-reminder>
```

`chat_history.jsonl` shows that Grok performed another assistant/LLM response after receiving this internal reminder, producing content substantially overlapping the answer from the real request.

### Guance trace data

The platform contains two distinct root traces for the overlapping answer:

- Trace `b4ae911b6659df6f3af25dd7d6879b3`: starts at 2026-08-28 10:40:19.563 UTC+08:00 and lasts 62.734 seconds.
- Trace `58ca4b446d5cf496e970fb30f177b8a1`: starts at 2026-08-28 10:41:22.309 UTC+08:00 and lasts 6.946 seconds.

The second trace starts approximately 12 milliseconds after the main trace completes. Their distinct trace IDs and prompt IDs rule out a duplicate retry of the same `(sessionId, promptId)`.

### Connector behavior

The diagnosed Grok parser defaulted normalized turns to `request_type=user_request` and only changed the type for detected subagents. It did not classify `task-completed-*` prompt IDs as internal traffic.

The diagnosed recovery/enqueue path accepted completed turns with journals and distinct prompt IDs. Therefore, deduplication worked as designed but could not recognize that the background-completion turn was a hidden runtime wake.

## Implemented correction

- Mirror Grok Build's exact, case-sensitive `PromptOrigin::from_prompt_id` prefixes. Hidden runtime wakes such as `task-completed-*`, `subagent-completed-*`, and `notifications-*` are suppressed before journaling or queueing.
- Use the initial persisted `user_message_chunk` `hideFromScrollback=true` flag as a structured forward-compatible fallback after `TurnCompleted`. Prompt text and timestamp proximity are not classification signals.
- Replace the active prompt context even for suppressed wakes so later tool Hooks without `promptId` cannot leak into the preceding human turn.
- Keep visible scheduler, plan-resume, and interject turns observable with explicit request types.
- Clean stale journal and queue entries created by older connector versions without uploading them.
- Add fixtures and regression tests for every known prompt origin, both observed metadata layouts, missing-prompt tool isolation, recovery, and no-upload behavior.

Hidden runtime continuation calls are intentionally omitted from the user-workflow export. They are real model calls, but presenting them as another `invoke_agent` would incorrectly claim that the user submitted another request. A future dedicated internal-runtime telemetry contract can expose that cost without changing user-turn semantics.
