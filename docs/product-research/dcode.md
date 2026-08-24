# Deep Agents Code Telemetry Product Research

## 1. Product Scope

- Product: Deep Agents Code, exposed by the `dcode` command.
- Locally validated versions: `deepagents-code 0.1.60` and `deepagents 0.7.8` on Linux x64.
- Minimum supported product version: `deepagents-code 0.1.46`, where Hooks v2 and transcript capability snapshots are both available.
- Supported connector platforms: macOS, Linux, and Windows. Product-level telemetry validation currently covers Linux x64 only.
- Target implementation: the built-in Dcode adapter in `obs-agent-connector`.
- Evidence date: 2026-08-24.

The validated scope covers interactive terminal and headless Dcode sessions that use the common Hook runtime. ACP/IDE-hosted execution has not yet been validated as a separate product surface.

## 2. Hook Capability

| Item | Conclusion | Evidence |
| --- | --- | --- |
| Extension mechanism | Hooks v2 in user or trusted project `hooks.json` | [Official Hooks reference](https://github.com/langchain-ai/deepagents/blob/main/libs/code/HOOKS.md) |
| User configuration | `~/.deepagents/hooks.json` | Official Hooks reference |
| Used events | `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `Stop`, `SubagentStart`, and `SubagentStop` | Official Hooks reference |
| Hook input | JSON on stdin with common session/transcript/cwd fields and event-specific prompt, tool, result, duration, error, or subagent fields | Official Hooks reference |
| Reload behavior | New sessions load Hooks; `/reload` refreshes capabilities in the current interactive session | [Official architecture reference](https://github.com/langchain-ai/deepagents/blob/main/libs/code/ARCHITECTURE.md) |
| Duplicate/concurrent events | No uniqueness guarantee is assumed | Connector journal locks and `(session_id, prompt_id)` upload claims |

Hooks v2 became the default execution engine in 0.1.45, while 0.1.46 added session capability snapshots and transcript metadata needed by this adapter. The connector therefore documents 0.1.46 as the minimum supported version. See the [official changelog](https://github.com/langchain-ai/deepagents/blob/main/libs/code/CHANGELOG.md).

## 3. Data Sources

| Data source | Path or entry | Format | Lifecycle | Sensitivity |
| --- | --- | --- | --- | --- |
| Hook | stdin | JSON | One connector process per event | Prompt, tool arguments/results, errors, final response |
| Transcript | Hook `transcript_path`; normally under `~/.deepagents/transcripts/` | JSONL schema v1 | Appended during the session | User, assistant, tool, and system messages |
| Connector journal | `~/.obs-agent-connector/dcode/state/journal/` | JSON | Reset on `UserPromptSubmit`, appended by later Hooks | Sanitized event evidence |
| Connector queue/state | `~/.obs-agent-connector/dcode/state/` | JSON | Persisted before upload | Normalized Turn and per-signal delivery state |

Transcript v1 records include sequence, record ID, thread ID, role, message ID, content, and optional agent/name fields. Dcode redacts credentials in its transcript store; the connector applies its own capture-mode sanitization again before persistence or upload.

## 4. Identifiers and Correlation

| Concept | Source field | Stability | Fallback |
| --- | --- | --- | --- |
| Session ID | Hook `session_id` | Stable for the Dcode session | No upload without a session ID |
| Turn ID | Hook `prompt_id` | Stable for interactive and headless turns | Derived hash of session and prompt evidence |
| Transcript thread | Transcript `thread_id` | Stable inside a session | Filter omitted when absent |
| Message ID | Transcript `message_id` | Stable when present | Derived from turn and sequence |
| LLM call ID | Assistant message ID | Stable when present | Derived from turn and assistant index |
| Tool call ID | Hook `tool_use_id` | Stable across pre/post events | Derived turn-local ID |
| Subagent ID | Hook `agent_id` | Stable across start/stop | Correlated with the task tool call when IDs match |

## 5. Lifecycle

- Turn start: `UserPromptSubmit`, using `prompt_id` as the authoritative turn identity.
- Tool success: `PreToolUse` followed by `PostToolUse`, with exact `duration_ms` supplied by Dcode.
- Tool failure: `PostToolUseFailure`, including error, interrupt status, and duration.
- Subagent lifecycle: `SubagentStart` and `SubagentStop` with stable agent ID/type.
- Normal terminal boundary: `Stop`, including `last_assistant_message` as a transcript-lag fallback.
- Cancellation or failure: inferred from explicit tool failure/interrupt evidence and any terminal evidence available to the Hook surface.
- Empty/internal records: transcript ranges without a user turn or assistant/tool evidence are not uploaded.

```text
UserPromptSubmit -> reset journal and record prompt
Pre/Post Hooks   -> append exact tool/subagent boundaries
Stop             -> append terminal evidence -> queue detached worker -> return
worker           -> transcript + journal -> normalized terminal Turn
                 -> spans -> metrics -> signal-specific upload state
```

## 6. LLM and Token Data

| Field | Source | Scope | Availability and limit |
| --- | --- | --- | --- |
| LLM call | Assistant transcript record | Call | One inferred call per assistant model output |
| Provider | Unknown | — | Omitted |
| Request/response model | Unknown on the Hook/transcript surface | — | Omitted |
| Input/output/cache/reasoning tokens | Unknown on the Hook/transcript surface | — | Omitted; no token metric is emitted |
| Finish reason | Terminal assistant evidence | Call | Normal completion is inferred only when terminal evidence exists |
| Start/end | Transcript and surrounding Hook boundaries | Call | Non-overlapping inferred slices, marked as inferred |
| TTFT | Unknown | — | Omitted |

The local session database contains LangGraph checkpoint internals with binary payloads. This adapter deliberately does not couple to that private storage or fabricate token allocation when the supported Hook/transcript surface provides none.

## 7. Tool, Skill, and Subagent Data

- Tool timing uses Dcode's `duration_ms`, anchored at Hook receipt time.
- Tool input/result/error fields are recursively redacted and bounded by capture mode.
- `PostToolUseFailure.is_interrupt` maps to interrupted tool status instead of a generic success.
- Subagent spans use `agent_id` and `agent_type`; when the ID equals the task tool call ID, the stable relationship is retained.
- The current supported surface has no reliable Skill event or path, so no `skill:*` span is emitted.

## 8. Installation and Configuration

| Platform | Product home | Hook file | Transcript store | Reload |
| --- | --- | --- | --- | --- |
| Linux | `~/.deepagents` | `~/.deepagents/hooks.json` | `~/.deepagents/transcripts/` | Start a new session or run `/reload` |
| macOS | `~/.deepagents` | Same | Same | Same; live validation pending |
| Windows | User home `.deepagents` | Same logical path | Same logical path | Same; live validation pending |

- Commands: `dcode`, with `deepagents-code` accepted as an alternate discovery command.
- Registry: the connector merges its seven managed Hook groups and preserves unrelated groups and handlers.
- Runtime config: `~/.obs-agent-connector/dcode/gtrace.json`, with `~/.deepagents/gtrace.json` as a migration fallback.
- Product write-back: Dcode owns its transcripts and other runtime state. The connector owns only its Hook handlers, managed config, journal, queue, upload state, and Hook log.
- Update behavior: Hook registration is reconciled with `--no-config`, preserving existing telemetry configuration.
- Sensitive config: endpoint/token headers stay in the mode-0600 managed config and are never written to Hook commands or logs.

## 9. Architecture Decision

- Pattern: hybrid Hook journal plus terminal transcript replay.
- OTLP: the repository's dependency-free OTLP/HTTP Protobuf encoder.
- Reason: Hooks provide exact tool timing, failure, subagent, and terminal boundaries; transcript replay supplies the multi-message/LLM sequence.
- Missing fields: provider/model/token/TTFT/Skill fields are omitted instead of inferred without evidence.
- Deduplication key: `(session_id, turn_id)` plus a normalized Turn fingerprint.
- Partial recovery: traces and metrics are marked independently. The normalized Turn is persisted before upload, allowing later Hooks to restart an interrupted worker and retry only missing signals.
- Host safety: Hook parsing, journaling, queueing, and export fail open and do not block Dcode.

## 10. Field Mapping

| Product field/event | Internal model | Span/attribute | Note |
| --- | --- | --- | --- |
| `UserPromptSubmit.prompt` | `Turn.InputMessages` | `invoke_agent` input | Redacted and bounded by capture mode |
| Assistant transcript record | `LLMCall` | `llm` | One call per assistant output |
| `PreToolUse` | Tool boundary | `tool:<name>` start | Uses `tool_use_id` |
| `PostToolUse` | `ToolCall.Result` | tool result/end | Uses supplied `duration_ms` |
| `PostToolUseFailure` | `ToolCall.Error` | error/interrupted status | Preserves sanitized error evidence |
| `SubagentStart/Stop` | `SubagentCall` | `subagent:<type>` | Stable `agent_id` correlation |
| `Stop.last_assistant_message` | `AssistantOutput` | `assistant` | Transcript-lag fallback |

## 11. Fixtures and Tests

All committed fixtures are synthetic and contain no real prompt, local user path, endpoint, or credential.

- [x] Hook v2 install merge and unrelated-handler preservation
- [x] Hook removal and unrelated-handler preservation
- [x] Normal question/answer transcript
- [x] Multi-LLM tool chain
- [x] Tool success with exact duration
- [x] Tool failure/interruption
- [x] Subagent correlation
- [x] Stop assistant-output fallback
- [x] Content-disabled and recursive secret redaction
- [x] Duplicate-safe and signal-specific upload recovery
- [x] Locally installed Dcode Hook config load and native `UserPromptSubmit` execution on Linux x64
- [ ] macOS and Windows live-session validation
- [ ] ACP/IDE-hosted session validation
- [ ] Skill event
- [ ] Product-supported per-call token evidence

## 12. Unknowns and Risks

| Question | Impact | Current fallback | Follow-up |
| --- | --- | --- | --- |
| Hook/transcript schema changes after the validated build | Parser may skip new fields or records | Ignore unknown fields and fail open | Revalidate on Dcode upgrades |
| Stop before the final transcript write is durable | Final transcript record may be missing | Use `last_assistant_message` and retain queued work | Validate retry timing against future builds |
| Provider/model/token visibility | Model and usage metrics are unavailable | Omit unsupported attributes and token metrics | Adopt a future public Hook/transcript field |
| Skill identity | No Skill spans | Omit unsupported relationships | Add only after Dcode exposes stable Skill evidence |
| ACP/IDE execution | Hooks may have host-specific lifecycle differences | Advertise terminal validation scope | Run separate ACP integration tests |
