# Kiro CLI Telemetry Product Research

## 1. Product Scope

- Product: Kiro CLI terminal agent.
- Locally validated binaries: `kiro-cli 2.18.1` legacy v3 storage and `kiro-cli 2.19.1` modern workspace-bucketed storage on Linux x64.
- Supported connector platforms: macOS, Linux, and Windows. Kiro officially supports the CLI on all three; product-level telemetry validation currently covers Linux x64 only.
- Target implementation: the built-in Kiro adapter in `obs-agent-connector`.
- Evidence date: 2026-08-25.

The connector does not instrument Kiro IDE sessions, default V2 sessions, non-interactive sessions, or legacy Kiro CLI v1/v2 SQLite sessions. The integration requires a V3 interactive TTY launched with `kiro-cli chat --v3`, because standalone global Hooks are a V3 surface. It supports both the modern 2.19.1 message journal and the legacy V3 JSONL layout after the required Hooks fire.

## 2. Hook Capability

| Item | Conclusion | Evidence |
| --- | --- | --- |
| Extension mechanism | V3 Command Hooks in global or project `.kiro/hooks/*.json` files | Kiro Hooks and CLI 3.0 documentation |
| Supported launch mode | Interactive TTY with `kiro-cli chat --v3` | Local 2.19.1 end-to-end validation and CLI 3.0 mode documentation |
| Unsupported launch modes | Default V2, `kiro-cli chat --no-interactive`, and V3 non-interactive compatibility paths | Standalone global Hooks are not invoked in local 2.19.1 validation |
| Used Hooks | `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, and `Stop` | Current Hook trigger reference |
| Hook input | JSON on stdin, including `session_id` and `cwd`; tool Hooks add tool name/input/response; Stop may omit `assistant_response` | Current CLI Hook reference and local 2.19.1 payloads |
| Timeout and failure | Command Hooks have a bounded timeout; nonzero exits are reported by Kiro | Current Hook action documentation |
| Duplicate/concurrent events | No uniqueness guarantee is assumed | Connector journal locks and `(session_id, turn_id)` upload claims |
| Replay | The exact session message journal is replayed after Stop | Local 2.19.1 and legacy V3 evidence |

## 3. Data Sources

| Data source | Path or entry | Format | Lifecycle | Sensitivity |
| --- | --- | --- | --- | --- |
| Hook | stdin | JSON | One process per event | Prompt, tool arguments, tool results, final response |
| Modern transcript | `~/.kiro/sessions/<workspace_hash>/<session_id>/messages.jsonl` | JSONL | Appended during a terminal session | Prompt, assistant output, request IDs, tool calls/results, terminal state |
| Modern session metadata | `~/.kiro/sessions/<workspace_hash>/<session_id>/session.json` | JSON | Rewritten as session metadata changes | Session ID, model, status, workspace paths |
| Modern session index | `~/.kiro/session-index/<workspace_hash>.jsonl` | JSONL | Updated as sessions change | Session discovery metadata |
| Legacy transcript | `~/.kiro/sessions/cli/<session_id>.jsonl` | JSONL v1 | Appended during a terminal session | Prompt, assistant output, tool calls/results |
| Legacy sidecar | `~/.kiro/sessions/cli/<session_id>.json` | JSON | Rewritten as session metadata changes | cwd, model, timing, aggregate token usage, result metadata |
| Legacy SQLite | `~/.local/share/kiro-cli/data.sqlite3` | SQLite | Used by older engines | Not read by this adapter |

Observed modern records include `user`, `turn_start`, `assistant`, `tool_call`, `tool_result`, `usage_summary`, and `turn_end`. Only user-visible `Say`, `Print`, and `Summary` assistant operations are retained; `Reasoning` is intentionally ignored. Legacy records include `Prompt`, `AssistantMessage`, and `ToolResults`.

## 4. Identifiers and Correlation

| Concept | Source field | Stability | Fallback |
| --- | --- | --- | --- |
| Session ID | Hook `session_id`; modern `session.json.id`; legacy sidecar `session_id` | Stable for the terminal conversation | No upload without an exact matching session ID |
| Turn ID | Modern `executionId`; legacy result/message ID | Stable in observed stores | Hash of session, start time, and prompt |
| Message ID | Modern record `id`; legacy assistant message ID | Stable inside a session | Not used as the modern Turn ID |
| LLM call ID | Modern `usage_summary.requestIds` in call order; legacy assistant message ID | Stable when present | Derived from the turn when absent |
| Tool call ID | Modern `toolCallId`; legacy `toolUseId` | Stable when present | Hook ID or a derived turn-local ID |
| Parent/subagent ID | Unknown | Not available on this surface | No subagent relationship is emitted |

## 5. Lifecycle

- Turn start: `UserPromptSubmit`, backed by a modern `user`/`turn_start` pair or a legacy `Prompt` record.
- Normal completion: `Stop`, backed by modern `turn_end.stopReason=end_turn` or the legacy sidecar end reason.
- Cancellation: modern `usage_summary.status=aborted` or terminal reasons containing cancelled, interrupted, or aborted; legacy end reasons use the same text mapping.
- Error: modern usage/terminal reasons or legacy end reasons containing error or failed.
- Internal reasoning records are excluded. A modern turn is not emitted until an explicit `turn_end` is present.
- Write ordering: Stop may arrive before the final session update is stable. The worker polls for up to two seconds. An incomplete JSONL tail is ignored while the last complete terminal turn remains readable.

```text
UserPromptSubmit -> journal prompt
PreToolUse       -> journal tool start
PostToolUse      -> journal tool result
Stop             -> snapshot journal -> persistent queue -> return immediately
worker           -> exact session metadata + JSONL -> normalized terminal Turn
                 -> spans -> metrics -> signal-specific upload state
```

## 6. LLM and Token Data

| Field | Source | Scope | Availability and limit |
| --- | --- | --- | --- |
| Provider | Derived from model name | Call | Omitted when it cannot be identified confidently |
| Request model | Modern `session.json.modelId` or legacy sidecar model | Turn | Used for each observed request ID/assistant-message call |
| Response model | Same as request model | Turn | No separate response field is available |
| Input token | Legacy `input_token_count` | Aggregate turn | Unavailable in the modern 2.19.1 journal |
| Output token | Legacy `output_token_count` | Aggregate turn | Unavailable in the modern 2.19.1 journal |
| Cache read token | Legacy `cache_read_input_token_count` | Aggregate turn | Unavailable in the modern 2.19.1 journal |
| Cache creation token | Legacy `cache_write_input_token_count` | Aggregate turn | Unavailable in the modern 2.19.1 journal |
| Billing credit | Modern `promptTurnSummaries[].usage` with an explicit `credit` or `credits` unit | Aggregate turn | Exported on `invoke_agent` as `gen_ai.usage.credit`; not emitted as a token metric |
| Reasoning token | Unknown | — | Omitted |
| Finish reason | Modern `turn_end.stopReason` or legacy sidecar end reason | Turn | Terminal status remains separate |
| Start/end | Modern record timestamps or legacy sidecar duration | Turn | Per-call windows are non-overlapping inferred slices |
| TTFT | Unknown | — | Omitted |

Modern `promptTurnSummaries` values with an explicit credit unit are exported on `invoke_agent` as `gen_ai.usage.credit`. Kiro exposes one aggregate turn value without a per-request breakdown, so the credit is not copied to individual `llm` spans. Credits are never mapped to token usage or `gen_ai.client.token.usage`. Legacy token metadata remains on a verified single `llm` call only; aggregate multi-call token metadata is not attributed to individual calls. Other Agent roots do not receive token or credit usage.

## 7. Tool, Skill, and Subagent Data

- Tool start/end: Hook receipt timestamps from `PreToolUse` and `PostToolUse` take precedence; modern record timestamps and `durationMs` or legacy inferred timing are fallbacks.
- Tool result/error: modern `tool_result` or legacy `ToolResults` takes precedence, then `PostToolUse.tool_response`; explicit success/error fields map to tool error status.
- Command extraction: `command`, `cmd`, or `script` from tool input after recursive redaction and truncation.
- Skill evidence: the current collection surface does not provide a reliable Skill event or path, so no `skill:*` span is emitted.
- Subagent model: unknown on this collection surface.
- Parent relationship: tool spans are direct children of `invoke_agent`; a triggering assistant message is referenced when available.

## 8. Installation and Configuration

| Platform | Product home | Hook file | Session store | Reload |
| --- | --- | --- | --- | --- |
| Linux | `~/.kiro` | `~/.kiro/hooks/obs-agent-connector.json` | `~/.kiro/sessions/<workspace_hash>/<session_id>`; legacy `~/.kiro/sessions/cli` | New sessions load the reconciled global Hook file |
| macOS | `~/.kiro` | Same | Same | Same; package-level telemetry validation remains pending |
| Windows | User home `.kiro` | Same logical path | Same logical path | Same; package-level telemetry validation remains pending |

- Official CLI command: `kiro-cli`.
- Required telemetry launch: `kiro-cli chat --v3` in a real TTY.
- Unsupported telemetry launches: default V2 and `--no-interactive`, even when combined with a V3 compatibility flag.
- Registry: standalone global Hook JSON; no marketplace is required.
- Runtime config: `~/.obs-agent-connector/kiro/gtrace.json`, with `~/.kiro/gtrace.json` as a migration fallback.
- Product write-back: Kiro owns its session files; the connector owns only its dedicated Hook file, managed config, journal, queue, upload state, and Hook log.
- Native/legacy conflict: legacy v1/v2 SQLite sessions are outside the supported surface. Legacy v3 JSONL remains readable for upgrade compatibility.
- Sensitive config: endpoint/token headers stay in the mode-0600 managed config and are never logged.

## 9. Architecture Decision

- Pattern: hybrid journal plus terminal replay.
- OTLP: the repository's dependency-free OTLP/HTTP Protobuf encoder.
- Reference patterns: WorkBuddy-style short Hook journal, CodeBuddy-style detached terminal worker, and shared connector state/semantic builders.
- Reason: transcript records provide message/tool identity and the authoritative terminal boundary while Hooks provide exact session correlation and tool timing.
- Missing events: transcript-only tools use product timestamps or inferred timing; Stop output fills a missing final transcript write when present; unknown Skill/subagent/token fields are omitted.
- Deduplication key: `(session_id, turn_id)` plus a normalized Turn fingerprint.
- Session safety: a Hook session ID must match `session.json.id` or the legacy sidecar filename/ID exactly. The adapter never falls back to a different session by cwd or modification time.
- Partial recovery: traces and metrics are marked independently. A persistent queue stores the normalized Turn before upload, so a later Hook can restart an interrupted worker and retry only the missing signal.

## 10. Field Mapping

| Product field/event | Internal model | Span/attribute | Note |
| --- | --- | --- | --- |
| Modern `user` / legacy `Prompt` | `Turn.InputMessages` | `invoke_agent` input | Redacted and bounded by capture mode |
| Modern `requestIds` / legacy `AssistantMessage` | `LLMCall` | `llm` | Reasoning/thinking content is excluded |
| Modern `tool_call` / legacy `ToolUse` | `ToolCall` | `tool:<name>` | Tool ID and arguments retained when allowed |
| Modern `tool_result` / legacy `ToolResults` / `PostToolUse` | `ToolCall.Result` | tool result attributes | Transcript result has precedence |
| Stop assistant response | `AssistantOutput` | `assistant` | Used as a transcript-lag fallback |
| Modern record timestamps / legacy sidecar duration | Turn window | root duration | Exact product evidence when present |
| Modern credit summaries | `Turn.CreditUsage` | `gen_ai.usage.credit` on `invoke_agent` | Aggregate turn credit; never copied to `llm` or token metrics |
| Legacy sidecar token counts | `LLMCall.Usage` | usage attributes/metrics on `llm` | Exported only for a verified single-call turn |

## 11. Fixtures and Tests

All committed fixtures are synthetic and contain no real prompt, user path, endpoint, or credential.

- [x] Normal question/answer
- [x] Multi-LLM tool chain
- [x] Tool success
- [x] Trace success plus metrics failure recovery
- [x] Incomplete final transcript with Stop fallback
- [x] Duplicate-safe upload state
- [x] Content disabled and recursive secret redaction
- [x] Modern 2.19.1 normal and multi-request turns
- [x] Modern aggregate billing credit on `invoke_agent` without per-LLM or token fabrication
- [x] Modern cancelled turn without assistant output
- [x] Incomplete modern JSONL tail
- [x] Exact-session mismatch does not fall back by cwd
- [x] Product-validated mode matrix: only V3 interactive TTY invokes the standalone global Hooks
- [ ] Product-validated tool failure payload
- [x] Product-validated cancelled terminal schema
- [ ] Skill event
- [ ] Subagent relationship
- [ ] macOS and Windows live-session validation

## 12. Unknowns and Risks

| Question | Impact | Current fallback | Follow-up |
| --- | --- | --- | --- |
| Kiro schema changes after the locally validated build | Parser may skip new record variants | Ignore unknown kinds; require an exact session and terminal turn | Revalidate on Kiro upgrades |
| Default V2 or non-interactive launch | Standalone global Hooks do not fire, so no exact terminal correlation is available | Advertise V3 interactive TTY as the supported telemetry surface | Revalidate if Kiro exposes global Hooks on additional engines or headless mode |
| Modern token usage | Token metrics are unavailable | Do not reinterpret billing credits as tokens | Capture a future explicit token field if Kiro exposes one |
| Modern multi-request credit attribution | Per-request credit is unavailable | Export aggregate credit on `invoke_agent` without assigning it to individual `llm` spans | Capture a future request ID on each credit summary if Kiro exposes one |
| Legacy aggregate usage for multi-call turns | Per-call token metrics are unavailable | Do not allocate aggregate tokens across calls | Capture a future per-call usage field if Kiro exposes one |
| Tool errors without an explicit error flag | Error status may be absent | Preserve the result and emit normal/unknown status | Capture a sanitized real failure fixture |
| Skill and subagent identity | No skill/subagent spans | Omit unsupported relationships | Revisit when Kiro exposes stable IDs/events |
| IDE session storage | Kiro IDE is not collected | Advertise CLI terminal scope only | Separate IDE research is required |
