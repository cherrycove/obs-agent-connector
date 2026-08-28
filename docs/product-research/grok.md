# Grok Build Telemetry Product Research

## 1. Product Scope

- Product: Grok Build CLI, exposed by the `grok` command.
- Stable compatibility baseline: [`xai-org/grok-build` commit `9fabadea800fa6e2ed8ec91c4f45f02b7e2504f4`](https://github.com/xai-org/grok-build/commit/9fabadea800fa6e2ed8ec91c4f45f02b7e2504f4), whose shell and pager crates report version 1.0.5 and whose Hook schema contains every event used by this adapter.
- Release-channel evidence: the [official installer](https://x.ai/cli/install.sh) defaults to the `stable` channel and resolves its version from `https://x.ai/cli/stable`; that pointer returned 1.0.5 on the evidence date. The default source branch reported 1.0.10 at the same time, so source-package versions are not used as the stable release floor.
- Minimum supported product version: Grok Build CLI 1.0.5.
- Supported connector platforms: macOS, Linux, and Windows.
- Supported product modes: TUI and headless sessions using Grok's common Hook and session-update surfaces.
- Target implementation: the built-in `grok` adapter in `obs-agent-connector`.
- Evidence date: 2026-08-27.

The product facts below were validated from the pinned upstream source and its documentation. The connector validation plan uses synthetic fixtures for the expected schemas, but a real Grok-to-collector end-to-end run has not yet been claimed for every platform.

## 2. Hook Capability

| Item | Conclusion | Evidence |
| --- | --- | --- |
| Extension mechanism | Command Hooks in global or project `.grok/hooks/*.json` files | [Pinned Custom Hooks guide](https://github.com/xai-org/grok-build/blob/9fabadea800fa6e2ed8ec91c4f45f02b7e2504f4/crates/codegen/xai-grok-pager/docs/custom-hooks.md) |
| Managed Hook location | `~/.grok/hooks/obs-agent-connector.json` | Global Hooks are always trusted; the connector owns one dedicated file |
| Used events | `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `PermissionDenied`, `Stop`, `StopFailure`, `StopCancelled`, `Notification(idle_prompt)`, `SubagentStart`, `SubagentStop`, and `SessionEnd` | [Pinned Hooks reference](https://github.com/xai-org/grok-build/blob/9fabadea800fa6e2ed8ec91c4f45f02b7e2504f4/crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md) |
| Hook input | camelCase JSON on stdin with common session, workspace, timestamp, transcript, permission, and optional prompt fields plus event-specific fields | [Pinned Hook envelope](https://github.com/xai-org/grok-build/blob/9fabadea800fa6e2ed8ec91c4f45f02b7e2504f4/crates/codegen/xai-grok-hooks/src/event.rs) |
| Reload behavior | Restart Grok, or run `/hooks`, select the Hooks tab, and press `l` | Pinned Custom Hooks guide |
| Host safety | Nonzero errors and timeouts fail open, except explicit gate decisions | Pinned Hooks reference |

The connector never returns a blocking decision. Each Hook process performs only bounded parsing, sanitization, local journaling, and worker scheduling before it exits 0.

## 3. Data Sources

| Data source | Path or entry | Format | Lifecycle | Sensitivity |
| --- | --- | --- | --- | --- |
| Hook | stdin | camelCase JSON | One connector process per event | Prompt, tool arguments/results, errors, final response |
| Session update stream | Hook `transcriptPath`; normally `~/.grok/sessions/<encoded-cwd>/<session-id>/updates.jsonl` | JSONL envelopes with Unix-second timestamp, method, and params | Append-only authoritative conversation/session stream | User, assistant, tool, model, and usage data |
| LLM conversation history | `chat_history.jsonl` next to the Hook `transcriptPath` | JSONL conversation items | Persisted input/output history used for LLM API calls | User messages, assistant messages, tool-call arguments/results, and model ID; no persisted per-call usage |
| Session event stream | `events.jsonl` next to the Hook `transcriptPath` | `xai-grok-session-events` JSONL, schema version `1.0`, with RFC 3339 timestamps | Append-only turn, loop, phase, first-token, tool, and terminal events | Session ID, model, timing, tool names, and outcomes |
| Connector journal | `~/.obs-agent-connector/grok/state/journal/` | Sanitized bounded JSON | Appended by Hooks and scoped to one turn | Capture-mode-limited Hook evidence |
| Connector queue/upload state | `~/.obs-agent-connector/grok/state/` | JSON | Persisted before detached processing and upload | Normalized terminal Turn and per-signal delivery markers |

The connector reads xAI `_x.ai/session/update` envelopes from `updates.jsonl`. The current extension surface includes durable `TurnCompleted` records and Messages-backend `ResponseStarted` / `ResponseCompleted` records. It also derives the sibling `events.jsonl` path and reads complete [`xai-grok-session-events` schema `1.0` turn blocks](https://github.com/xai-org/grok-build/blob/9fabadea800fa6e2ed8ec91c4f45f02b7e2504f4/crates/codegen/xai-grok-session-events/src/types.rs). For captured content, a turn's `session/update` `promptIndex` selects the matching block in `chat_history.jsonl`; the number of assistant responses must exactly match the already-proven LLM call count before any call is enriched. Unknown records and an incomplete final JSONL line are ignored rather than making the Hook fail.

## 4. Identifiers and Correlation

| Concept | Source field | Stability | Fallback |
| --- | --- | --- | --- |
| Session ID | Hook `sessionId` | Stable for a Grok session | No upload without a session ID |
| Turn ID | Hook/transcript `promptId` / `prompt_id` | Opaque turn key, scoped to the session | Recovery scans terminal records by session; no clock-derived identity |
| LLM call ID | `ResponseStarted.message_id`, completed by matching `ResponseCompleted.message_id` | Stable on the Messages backend | Turn-local derived ID for validated `events.jsonl` loops or explicitly synthetic Hook-boundary estimates |
| Tool call ID | `toolUseId` | Stable across pre/post tool Hooks | Derived turn-local ID only when correlation evidence is otherwise unambiguous |
| Subagent ID | `subagentId` | Stable across start/stop Hooks | No timing-only relationship |

Some current tool Hook call sites may omit `promptId`. The adapter therefore records the active prompt per session from `UserPromptSubmit` and uses it only as the turn-local fallback. Turn-end events may arrive after the next prompt, so explicit prompt IDs take precedence over receipt time.

## 5. Lifecycle and Terminality

- Turn start: `UserPromptSubmit`, using `(sessionId, promptId)` as the primary key.
- Tool lifecycle: `PreToolUse` followed by `PostToolUse`, `PostToolUseFailure`, or `PermissionDenied`.
- Normal completion signal: `Stop(reason=end_turn)`, subject to the durable transcript check below.
- Failure: `StopFailure`, with one of `rate_limit`, `authentication_failed`, `invalid_request`, `server_error`, `max_output_tokens`, or `unknown`.
- Cancellation: `StopCancelled`, including `user_interrupt`, `permission_rejected`, `permission_cancelled`, `max_turns`, `no_progress`, or `unknown`.
- Recovery triggers: a later `UserPromptSubmit`, `Notification(notificationType=idle_prompt)`, and `SessionEnd` scan for terminal turns missed by an observe Hook.

`Stop` is a gate. It can be blocked and repeated, and `stopHookActive=true` does not distinguish the final continuation from an intermediate one. The connector therefore uploads a normal turn only after finding a matching durable `TurnCompleted` record in `updates.jsonl`. A session-end Stop with no prompt ID and reason `channel_closed` or `shutdown` is not treated as a turn.

```text
Hook event       -> sanitize and append per-(session,prompt) journal
terminal/recovery -> enqueue local work and return immediately
worker           -> exact transcript + matching TurnCompleted
                 -> normalized terminal Turn
                 -> spans -> metrics -> signal-specific upload state
```

## 6. LLM and Token Data

| Field | Source | Scope | Availability and limit |
| --- | --- | --- | --- |
| Request/message ID | `ResponseStarted.message_id` | Call | Messages backend only |
| Model | `ResponseStarted.model` | Call | Omitted when absent; provider is not inferred from a custom model name |
| Input token | `ResponseStarted.input_tokens` | Call | Messages backend only |
| Cache read/create token | `ResponseStarted.cache_read_input_tokens` / `cache_creation_input_tokens` | Call | Messages backend only |
| Output/final usage | `ResponseCompleted.usage` | Call | Requires a stable response pair |
| Finish reason | `ResponseCompleted.stop_reason` | Call | Verbatim when present |
| Model request boundary | schema `1.0` `phase_changed(waiting_for_model)` through `phase_changed(tool_execution)`, the next `loop_started`, or `turn_ended` | Call | Used only from one complete turn block that uniquely matches the Hook session and bounded Hook-delivery skew; the root expands to the official turn boundary |
| TTFT | schema `1.0` `first_token` following the active `waiting_for_model` phase | Call | Omitted when a call produces no text/reasoning token before a tool request |
| Model | schema `1.0` `turn_started.model_id` | Turn and validated calls | Used as the request model for every event-derived call in that turn |
| Turn outcome | `TurnCompleted.stop_reason`, `agent_result`, and optional aggregate usage | Turn | Authoritative terminal evidence; aggregate usage is written to the root span and is not duplicated across calls |
| LLM input/output | prompt-indexed block in `chat_history.jsonl` | Call | User input populates the first call, tool results populate subsequent calls, and each assistant item populates one output; accepted only on an exact call-count match |

`ResponseStarted` and `ResponseCompleted` are documented in the pinned source as Messages-backend-only. The connector uses the following strict precedence:

1. Exact `ResponseStarted` / `ResponseCompleted` pairs provide the preferred per-call model, boundary, finish reason, and token usage.
2. Without exact response pairs, exactly one complete schema `1.0` event turn must match the Hook session and both Hook endpoints within a two-second delivery-skew bound. The root expands to the official event turn boundaries. Its `waiting_for_model` phases provide real call boundaries and `first_token` provides TTFT. The `TurnCompleted.usage.modelCalls` count must equal the number of event-derived calls. When that count is exactly one, the turn prompt, final output, and aggregate usage also belong to the sole call and populate its LLM span. With multiple calls, aggregate tokens remain only on the root because the event stream has no per-call token split.
3. Without usable event evidence, one `llm` span can still come from complete aggregate usage when `modelCalls=1`, exactly one `modelUsage` entry confirms that call, and `apiDurationMs` provides its duration.
4. For multiple calls, the last fallback requires complete aggregate usage, one model entry whose count matches `modelCalls`, positive `apiDurationMs`, and exactly `modelCalls-1` non-overlapping clusters formed from complete tool intervals. It places one estimated LLM call in every causal gap, fits the aggregate API duration without crossing a tool cluster, associates every clustered tool with the preceding estimated call, and marks the spans with `timing.source=grok_hook_boundaries`, `gtrace.synthetic=true`, and `gtrace.timing.estimated=true`. Any count, interval, model, or duration mismatch disables this fallback.

After call boundaries are established, `chat_history.jsonl` enriches each call with the real incremental input and assistant output. Tool-call arguments are parsed before recursive secret redaction, and all content still follows `captureContent` and `maxChars`. The persisted assistant records contain model identity and content but no per-call token usage.

The connector never distributes aggregate turn tokens across multiple event-derived or synthetic calls. This prevents unproven per-call token attributes and token metrics while preserving the reliable aggregate on `invoke_agent`. The root also exposes `usage_input_tokens` and `usage_output_tokens` compatibility aliases for Agent Monitoring views while retaining the canonical GenAI attributes as the semantic source of truth.

## 7. Tool, Skill, and Subagent Data

- Tool input, result, and error use the Hook payload after recursive redaction and capture-mode truncation.
- `durationMs`, when present, is preferred. Otherwise the adapter uses observed pre/post Hook boundaries and marks no stronger timing guarantee.
- A tool is linked to an exact response LLM only when its complete Hook interval follows a `tool_use` response and ends no later than the next response start. An event-derived link likewise requires the complete interval to fall after that call and before the next call. The synthetic Hook-cluster fallback links every tool in a validated execution cluster to the estimated LLM immediately before it.
- A `skill:*` span requires high-confidence evidence such as a tool input path ending in `SKILL.md`; an arbitrary mention of a skill name is not enough.
- `SubagentStart` and `SubagentStop` correlate by `subagentId` and type. A child trace is emitted only when a stable child session/transcript exists; time proximity alone never creates a parent link.

## 8. Installation and Configuration

| Platform | Product home | Hook file | Session store | Reload |
| --- | --- | --- | --- | --- |
| macOS | `~/.grok` | `~/.grok/hooks/obs-agent-connector.json` | `~/.grok/sessions/` | Restart or `/hooks` → Hooks tab → `l`; headless 1.0.5 E2E verified on 2026-08-27 |
| Linux | `~/.grok` | Same | Same | Same; live E2E pending |
| Windows | User home `.grok` | Same logical path | Same logical path | Same; live E2E pending |

- Discovery: the `grok` executable in `PATH` or an existing `~/.grok` home.
- Version policy: reject a known version below 1.0.5; allow an unknown/unparseable version with a warning.
- Runtime config: `~/.obs-agent-connector/grok/gtrace.json`.
- Hook log: `~/.obs-agent-connector/grok/gtrace-hooks.json`.
- Journal, queue, and upload state: `~/.obs-agent-connector/grok/state/`.
- Removal: delete only the dedicated connector Hook and connector-managed Grok directory; preserve every unrelated Grok Hook.

## 9. Architecture Decision

- Pattern: hybrid Hook journal plus terminal `updates.jsonl` and sibling `events.jsonl` replay.
- Reason: Hooks supply exact lifecycle and tool boundaries, `TurnCompleted` prevents blocked/repeated Stop events from exporting partial turns, Response records provide exact per-call usage when available, and the schema-versioned event stream provides model/phase/TTFT boundaries on backends that omit Response records.
- Deduplication: `(session_id, prompt_id)` plus the normalized Turn fingerprint.
- Partial recovery: trace and metrics delivery markers are independent, so a successful signal is not resent when only the other signal failed.
- Privacy: `enabled=false` exits before stdin is read. Hook input is capped at 128 KiB, content follows `none|preview|full` and `maxChars`, and secret-like keys are recursively redacted before persistence.
- Resource defaults: `service.name=gtrace-grok`, `agent_runtime=grok`, `agent_name=Grok Build`, and the detected Grok version when available. The connector version is recorded on the instrumentation scope.

## 10. Field Mapping

| Product field/event | Internal model | Span/attribute | Note |
| --- | --- | --- | --- |
| `UserPromptSubmit.prompt` | `Turn.InputMessages` | `invoke_agent` input | Redacted and bounded by capture mode |
| Response start/completion pair | `LLMCall` | `llm` | Per-call model and tokens only with stable evidence |
| prompt-indexed `chat_history.jsonl` block | `LLMCall` content | `llm` input/output | Exact assistant-count match required; never used as token evidence |
| schema `1.0` event turn | `LLMCall` | `llm` | Real phase boundary and optional TTFT; aggregate tokens stay on root |
| validated aggregate usage plus tool clusters | `LLMCall` | `llm` | Explicitly synthetic estimated timing; no per-call tokens |
| `PreToolUse` plus terminal tool event | `ToolCall` | `tool:<name>` | Tool ID correlation; explicit failure/denial preserved |
| matching `SKILL.md` tool evidence | Skill call | `skill:<name>` under its tool | No name-only inference |
| `SubagentStart` / `SubagentStop` | Subagent call | subagent tool lifecycle | Stable ID required |
| `TurnCompleted.agent_result` | `AssistantOutput` | `assistant` | Final terminal output |
| `StopFailure` | Turn error | error `invoke_agent` | Keeps classified error; no fabricated LLM usage |
| `StopCancelled` | Cancelled Turn | cancelled `invoke_agent` | Keeps reason and actor when available |

## 11. Validation Matrix

All committed connector fixtures must be synthetic and contain no real prompt, user path, endpoint, or credential.

- Hook install/update/remove preservation and reload guidance.
- Normal and multi-response terminal turns, schema `1.0` event-derived calls, and conservative Hook-cluster fallback/rejection cases.
- Tool success, failure, and permission denial.
- Blocked/repeated Stop and session-end Stop exclusion.
- Failure, cancellation, cancel-and-send recovery, and out-of-order end reports.
- Tool Hooks without `promptId` and an incomplete transcript tail.
- Conservative Skill and subagent positive/negative cases.
- Duplicate/concurrent Hook delivery and trace/metrics partial retry.
- Build, unit tests, static checks, and six release package targets.
- Real Grok 1.0.5 headless collector validation is recorded on macOS; TUI mode plus Linux and Windows remain release follow-ups.

## 12. Native External OpenTelemetry Coexistence

Grok 1.0.5 also has an alpha, double-opt-in External OpenTelemetry stream controlled by `GROK_EXTERNAL_OTEL` plus explicit log/metric exporters. The pinned [Monitoring Usage guide](https://github.com/xai-org/grok-build/blob/9fabadea800fa6e2ed8ec91c4f45f02b7e2504f4/crates/codegen/xai-grok-pager/docs/user-guide/24-monitoring-usage.md) states that it exports logs and metrics only, with no customer-facing trace exporter.

The connector does not enable, disable, or rewrite that native configuration. Both streams can coexist, but enabling both can increase or overlap telemetry volume. Use the connector when GTrace turn traces, its span hierarchy, and its derived metrics are required.

## 13. Unknowns and Risks

| Question | Impact | Current fallback | Follow-up |
| --- | --- | --- | --- |
| Hook or transcript schema changes after the pinned commit | New records may be skipped | Ignore unknown fields and fail open; require terminal evidence | Revalidate on Grok upgrades |
| Response records absent on a non-Messages backend | Per-call token usage remains unavailable | Use unambiguous schema `1.0` event timing when available; otherwise use the marked Hook-cluster estimate only when every evidence gate passes | Adopt a future public per-call usage record |
| Agent Monitoring summary reads token fields only from `llm` spans | Multi-call trace summary can show `-` even though the root contains the exact turn aggregate | Keep the aggregate on `invoke_agent`; do not assign it to an arbitrary call or duplicate it across calls | Make the view fall back to root aggregate usage for multi-call turns |
| Event schema changes or multiple event turns match one Hook window | Event-derived call boundaries become ambiguous | Accept only schema `1.0` and exactly one session turn whose endpoints satisfy the bounded Hook-delivery skew; fall back conservatively or omit calls | Revalidate before accepting a new schema |
| Stop arrives before transcript durability | Turn may initially be incomplete | Keep queued work and retry on later Hooks | Validate polling bounds with live sessions |
| Replaced turn emits no cancellation Hook | Observe Hook alone can miss it | Scan `TurnCompleted` on prompt, idle, and session recovery events | Exercise live cancel-and-send behavior |
| Cross-platform product differences | Paths, process detachment, or Hook shell behavior may vary | Connector packages all three platforms without claiming live validation | Record real TUI/headless E2E on each platform |
