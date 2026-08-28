# Grok Build Telemetry Product Research

## 1. Product Scope

- Product: Grok Build CLI, exposed by the `grok` command.
- Source baseline: [`xai-org/grok-build` commit `77cd7eb675ba911c225c3aaeeece3a20cbccc426`](https://github.com/xai-org/grok-build/commit/77cd7eb675ba911c225c3aaeeece3a20cbccc426), whose shell and pager crates report version 1.0.10.
- Minimum supported product version: Grok Build CLI 1.0.10.
- Supported connector platforms: macOS, Linux, and Windows.
- Supported product modes: TUI and headless sessions using Grok's common Hook and session-update surfaces.
- Target implementation: the built-in `grok` adapter in `obs-agent-connector`.
- Evidence date: 2026-08-27.

The product facts below were validated from the pinned upstream source and its documentation. The connector validation plan uses synthetic fixtures for the expected schemas, but a real Grok-to-collector end-to-end run has not yet been claimed for every platform.

## 2. Hook Capability

| Item | Conclusion | Evidence |
| --- | --- | --- |
| Extension mechanism | Command Hooks in global or project `.grok/hooks/*.json` files | [Pinned Custom Hooks guide](https://github.com/xai-org/grok-build/blob/77cd7eb675ba911c225c3aaeeece3a20cbccc426/crates/codegen/xai-grok-pager/docs/custom-hooks.md) |
| Managed Hook location | `~/.grok/hooks/obs-agent-connector.json` | Global Hooks are always trusted; the connector owns one dedicated file |
| Used events | `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `PermissionDenied`, `Stop`, `StopFailure`, `StopCancelled`, `Notification(idle_prompt)`, `SubagentStart`, `SubagentStop`, and `SessionEnd` | [Pinned Hooks reference](https://github.com/xai-org/grok-build/blob/77cd7eb675ba911c225c3aaeeece3a20cbccc426/crates/codegen/xai-grok-pager/docs/user-guide/10-hooks.md) |
| Hook input | camelCase JSON on stdin with common session, workspace, timestamp, transcript, permission, and optional prompt fields plus event-specific fields | [Pinned Hook envelope](https://github.com/xai-org/grok-build/blob/77cd7eb675ba911c225c3aaeeece3a20cbccc426/crates/codegen/xai-grok-hooks/src/event.rs) |
| Reload behavior | Restart Grok, or run `/hooks`, select the Hooks tab, and press `l` | Pinned Custom Hooks guide |
| Host safety | Nonzero errors and timeouts fail open, except explicit gate decisions | Pinned Hooks reference |

The connector never returns a blocking decision. Each Hook process performs only bounded parsing, sanitization, local journaling, and worker scheduling before it exits 0.

## 3. Data Sources

| Data source | Path or entry | Format | Lifecycle | Sensitivity |
| --- | --- | --- | --- | --- |
| Hook | stdin | camelCase JSON | One connector process per event | Prompt, tool arguments/results, errors, final response |
| Session update stream | Hook `transcriptPath`; normally `~/.grok/sessions/<encoded-cwd>/<session-id>/updates.jsonl` | JSONL envelopes with Unix-second timestamp, method, and params | Append-only authoritative conversation/session stream | User, assistant, tool, model, and usage data |
| Connector journal | `~/.obs-agent-connector/grok/state/journal/` | Sanitized bounded JSON | Appended by Hooks and scoped to one turn | Capture-mode-limited Hook evidence |
| Connector queue/upload state | `~/.obs-agent-connector/grok/state/` | JSON | Persisted before detached processing and upload | Normalized terminal Turn and per-signal delivery markers |

The connector reads xAI `_x.ai/session/update` envelopes from `updates.jsonl`. The current extension surface includes durable `TurnCompleted` records and Messages-backend `ResponseStarted` / `ResponseCompleted` records. Unknown records and an incomplete final JSONL line are ignored rather than making the Hook fail.

## 4. Identifiers and Correlation

| Concept | Source field | Stability | Fallback |
| --- | --- | --- | --- |
| Session ID | Hook `sessionId` | Stable for a Grok session | No upload without a session ID |
| Turn ID | Hook/transcript `promptId` / `prompt_id` | Opaque turn key, scoped to the session | Recovery scans terminal records by session; no clock-derived identity |
| LLM call ID | `ResponseStarted.message_id`, completed by matching `ResponseCompleted.message_id` | Stable on the Messages backend | No fabricated per-call identity when the pair is unavailable |
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
| Turn outcome | `TurnCompleted.stop_reason`, `agent_result`, and optional aggregate usage | Turn | Authoritative terminal evidence; aggregate usage is not duplicated across calls |

`ResponseStarted` and `ResponseCompleted` are documented in the pinned source as Messages-backend-only. For another backend, the connector still emits the root, tools, and assistant output when supported, but omits unproven per-call model/token data. It never distributes aggregate turn tokens across multiple LLM calls.

## 7. Tool, Skill, and Subagent Data

- Tool input, result, and error use the Hook payload after recursive redaction and capture-mode truncation.
- `durationMs`, when present, is preferred. Otherwise the adapter uses observed pre/post Hook boundaries and marks no stronger timing guarantee.
- A `skill:*` span requires high-confidence evidence such as a tool input path ending in `SKILL.md`; an arbitrary mention of a skill name is not enough.
- `SubagentStart` and `SubagentStop` correlate by `subagentId` and type. A child trace is emitted only when a stable child session/transcript exists; time proximity alone never creates a parent link.

## 8. Installation and Configuration

| Platform | Product home | Hook file | Session store | Reload |
| --- | --- | --- | --- | --- |
| macOS | `~/.grok` | `~/.grok/hooks/obs-agent-connector.json` | `~/.grok/sessions/` | Restart or `/hooks` → Hooks tab → `l`; live E2E pending |
| Linux | `~/.grok` | Same | Same | Same; live E2E pending |
| Windows | User home `.grok` | Same logical path | Same logical path | Same; live E2E pending |

- Discovery: the `grok` executable in `PATH` or an existing `~/.grok` home.
- Version policy: reject a known version below 1.0.10; allow an unknown/unparseable version with a warning.
- Runtime config: `~/.obs-agent-connector/grok/gtrace.json`.
- Hook log: `~/.obs-agent-connector/grok/gtrace-hooks.json`.
- Journal, queue, and upload state: `~/.obs-agent-connector/grok/state/`.
- Removal: delete only the dedicated connector Hook and connector-managed Grok directory; preserve every unrelated Grok Hook.

## 9. Architecture Decision

- Pattern: hybrid Hook journal plus terminal `updates.jsonl` replay.
- Reason: Hooks supply exact lifecycle and tool boundaries, while `TurnCompleted` prevents blocked/repeated Stop events from exporting partial turns and Response records provide the only stable per-call usage evidence.
- Deduplication: `(session_id, prompt_id)` plus the normalized Turn fingerprint.
- Partial recovery: trace and metrics delivery markers are independent, so a successful signal is not resent when only the other signal failed.
- Privacy: `enabled=false` exits before stdin is read. Hook input is capped at 128 KiB, content follows `none|preview|full` and `maxChars`, and secret-like keys are recursively redacted before persistence.
- Resource defaults: `service.name=gtrace-grok`, `agent_runtime=grok`, `agent_name=Grok Build`, and the detected Grok version when available. The connector version is recorded on the instrumentation scope.

## 10. Field Mapping

| Product field/event | Internal model | Span/attribute | Note |
| --- | --- | --- | --- |
| `UserPromptSubmit.prompt` | `Turn.InputMessages` | `invoke_agent` input | Redacted and bounded by capture mode |
| Response start/completion pair | `LLMCall` | `llm` | Per-call model and tokens only with stable evidence |
| `PreToolUse` plus terminal tool event | `ToolCall` | `tool:<name>` | Tool ID correlation; explicit failure/denial preserved |
| matching `SKILL.md` tool evidence | Skill call | `skill:<name>` under its tool | No name-only inference |
| `SubagentStart` / `SubagentStop` | Subagent call | subagent tool lifecycle | Stable ID required |
| `TurnCompleted.agent_result` | `AssistantOutput` | `assistant` | Final terminal output |
| `StopFailure` | Turn error | error `invoke_agent` | Keeps classified error; no fabricated LLM usage |
| `StopCancelled` | Cancelled Turn | cancelled `invoke_agent` | Keeps reason and actor when available |

## 11. Validation Matrix

All committed connector fixtures must be synthetic and contain no real prompt, user path, endpoint, or credential.

- Hook install/update/remove preservation and reload guidance.
- Normal and multi-response terminal turns.
- Tool success, failure, and permission denial.
- Blocked/repeated Stop and session-end Stop exclusion.
- Failure, cancellation, cancel-and-send recovery, and out-of-order end reports.
- Tool Hooks without `promptId` and an incomplete transcript tail.
- Conservative Skill and subagent positive/negative cases.
- Duplicate/concurrent Hook delivery and trace/metrics partial retry.
- Build, unit tests, static checks, and six release package targets.
- Real Grok TUI/headless collector validation on macOS, Linux, and Windows remains a release follow-up until recorded separately.

## 12. Native External OpenTelemetry Coexistence

Grok 1.0.10 also has an alpha, double-opt-in External OpenTelemetry stream controlled by `GROK_EXTERNAL_OTEL` plus explicit log/metric exporters. The pinned [Monitoring Usage guide](https://github.com/xai-org/grok-build/blob/77cd7eb675ba911c225c3aaeeece3a20cbccc426/crates/codegen/xai-grok-pager/docs/user-guide/24-monitoring-usage.md) states that it exports logs and metrics only, with no customer-facing trace exporter.

The connector does not enable, disable, or rewrite that native configuration. Both streams can coexist, but enabling both can increase or overlap telemetry volume. Use the connector when GTrace turn traces, its span hierarchy, and its derived metrics are required.

## 13. Unknowns and Risks

| Question | Impact | Current fallback | Follow-up |
| --- | --- | --- | --- |
| Hook or transcript schema changes after the pinned commit | New records may be skipped | Ignore unknown fields and fail open; require terminal evidence | Revalidate on Grok upgrades |
| Response records absent on a non-Messages backend | Per-call model and tokens unavailable | Omit unsupported LLM attributes/metrics | Adopt a future public per-call record |
| Stop arrives before transcript durability | Turn may initially be incomplete | Keep queued work and retry on later Hooks | Validate polling bounds with live sessions |
| Replaced turn emits no cancellation Hook | Observe Hook alone can miss it | Scan `TurnCompleted` on prompt, idle, and session recovery events | Exercise live cancel-and-send behavior |
| Cross-platform product differences | Paths, process detachment, or Hook shell behavior may vary | Connector packages all three platforms without claiming live validation | Record real TUI/headless E2E on each platform |
