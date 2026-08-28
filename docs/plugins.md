# Plugin Matrix

`obs-agent-connector` contains built-in adapters for Claude, CodeBuddy, Codex, Cursor, Deep Agents Code, Grok Build, and Kiro CLI. Other Agents delegate installation and configuration generation to external plugin installers.

## Supported Agents

| Agent | Edition | Installer | Default Config | Default Install Marker |
| --- | --- | --- | --- | --- |
| `claude` | Claude | Current connector | `~/.obs-agent-connector/claude/gtrace.json` | Managed Hooks in `~/.claude/settings.json` |
| `codebuddy` | Tencent Cloud CodeBuddy / WorkBuddy Enterprise IDE Agent | Current connector | `~/.obs-agent-connector/codebuddy/gtrace.json` | Managed Hook in `~/.codebuddy/settings.json` |
| `codex` | Codex | Current connector | `~/.obs-agent-connector/codex/gtrace.json` | Managed Hook and trust state in `~/.codex/hooks.json` / `~/.codex/config.toml` |
| `cursor` | Cursor with automatic `~/.cursor` or Cursor CLI-family detection, preferring `cursor-agent` | Current connector | `~/.obs-agent-connector/cursor/gtrace.json` | Managed Hooks in `~/.cursor/hooks.json` |
| `dcode` | Deep Agents Code with Hooks v2 (`dcode` 0.1.46 or later); normal `Stop` plus failed `SessionEnd` terminal telemetry | Current connector | `~/.obs-agent-connector/dcode/gtrace.json` | Managed Hooks in `~/.deepagents/hooks.json` |
| `grok` | Grok Build CLI 1.0.5+ TUI/headless | Current connector | `~/.obs-agent-connector/grok/gtrace.json` | Managed global Hook in `~/.grok/hooks/obs-agent-connector.json` |
| `kiro` | Kiro CLI V3 interactive TTY (`kiro-cli chat --v3`); default V2 and non-interactive modes are unsupported | Current connector | `~/.obs-agent-connector/kiro/gtrace.json` | Managed V3 global Hooks in `~/.kiro/hooks/obs-agent-connector.json` |
| `dsh` | DeepSeek Harness | Unix: `https://static.guance.com/agent_plugins/dsh-otel-plugin/install.sh` Windows: `https://static.guance.com/agent_plugins/dsh-otel-plugin/install-release.ps1` | `$DSH_HOME/gtrace.json` (default `~/.dsh/gtrace.json`) | `$DSH_HOME/profiles/<profile>/node_modules/dsh-otel-plugin` |
| `hermes` | Hermes | `https://static.guance.com/agent_plugins/hermes-otel-plugin/install.sh` | `~/.hermes/config.yaml` | `~/.hermes/plugins/hermes-otel-plugin` |
| `opencode` | OpenCode with automatic config-directory detection | Unix: `https://static.guance.com/agent_plugins/opencode-otel-plugin/opencode-otel-plugin.tar.gz`  Windows: `https://static.guance.com/agent_plugins/opencode-otel-plugin/install-release.ps1` | `~/.config/opencode/gtrace.json` | `~/.config/opencode/plugins/opencode-otel-plugin` |
| `openclaw` | OpenClaw | Unix: `https://static.guance.com/agent_plugins/openclaw-otel-plugin/install.sh`  Windows: `https://static.guance.com/agent_plugins/openclaw-otel-plugin/install-release.ps1` | `~/.openclaw/openclaw.json` | `~/.openclaw/extensions/openclaw-otel-plugin` |
| `qoder` | Qoder with automatic CN/global detection | Unix: `https://static.guance.com/agent_plugins/qoder-otel-plugin/qoder-otel-plugin.tar.gz`  Windows: `https://static.guance.com/agent_plugins/qoder-otel-plugin/install-release.ps1` | `~/.qoder/gtrace.json` or `~/.qoder-cn/gtrace.json` | `~/.qoder/plugins/cache/qoder-marketplace/qoder-otel-plugin` or `~/.qoder-cn/plugins/cache/qoder-marketplace/qoder-otel-plugin` |
| `workbuddy` | WorkBuddy with automatic profile-directory detection | macOS: `https://static.guance.com/agent_plugins/workbuddy-otel-plugin/workbuddy-otel-plugin.tar.gz`  Windows: `https://static.guance.com/agent_plugins/workbuddy-otel-plugin/install-release.ps1` | `~/.workbuddy/gtrace.json` | `~/.workbuddy/plugins/marketplaces/guance/plugins/workbuddy-otel-plugin` |

## Grok Build

The Grok adapter uses a hybrid journal and transcript-replay design. Hook handlers return quickly after recording bounded evidence. A detached worker uses the matching `updates.jsonl` terminal record to normalize and upload a completed turn.

- `Stop` is a blocking gate and may fire repeatedly. The connector requires a matching durable `TurnCompleted` record, so a blocked or repeated `Stop` does not upload a partial turn.
- `StopFailure` and `StopCancelled` preserve explicit failure or cancellation evidence. A later `UserPromptSubmit`, `idle_prompt` notification, or `SessionEnd` also recovers terminal turns that did not receive a final observable Stop event.
- Per-call model and token fields are emitted only when `ResponseStarted` and `ResponseCompleted` provide stable call evidence. Turn totals are not copied across multiple LLM spans.
- Skill and subagent spans require stable IDs or a high-confidence `SKILL.md` path. The connector does not infer relationships from timing alone.
- Grok's native External OpenTelemetry stream may run at the same time. It exports logs and metrics, while the connector provides GTrace traces and derived metrics; enabling both can produce overlapping telemetry volume.

The connector owns `~/.grok/hooks/obs-agent-connector.json` and `~/.obs-agent-connector/grok/`. It preserves other global and project Hook files. Restart Grok after installation, or run `/hooks`, select the Hooks tab, and press `l` to reload.

See [Grok Build telemetry product research](product-research/grok.md) for schema and validation details.

## Qoder Variants

Both `qoder` and `qoder-cn` use the same plugin installer:

| Agent | Behavior |
| --- | --- |
| `qoder` | Detects the local layout, sets `QODER_HOME`, and passes `--variant cn` or `--variant global` |
| `qoder-cn` | Legacy compatibility target that forces the CN layout with `QODER_HOME=~/.qoder-cn` and `--variant cn` |

Qoder discovery requires an existing `~/.qoder` or `~/.qoder-cn` directory. If neither directory exists, the Agent is treated as not installed and its plugin is not installed.

This prevents the international and China editions from overwriting each other's plugin files and configuration.

## Windows Support

Windows installation and update are currently supported only for:

- `claude`
- `codex`
- `cursor`
- `dcode`
- `grok`
- `kiro`
- `codebuddy`
- `dsh`
- `opencode`
- `openclaw`
- `qoder`
- `workbuddy`

Claude, CodeBuddy, Codex, Cursor, Deep Agents Code, Grok Build, and Kiro register the current connector executable directly. External plugins download their PowerShell installer from the configured OSS or GitHub source.
If a user tries `install` or `update` with an unsupported Agent, the CLI returns a friendly error with the supported Windows Agent list.

## Install Parameters

Bootstrap stores shared defaults for `Endpoint` and `X-Token` in `~/.obs-agent-connector/config.json`.
At plugin install time, the CLI uses:

| Value | Source | Plugin Argument |
| --- | --- |
| `Endpoint` | `config.json` or `--endpoint` override | `--endpoint` |
| `X-Token` | `config.json` or `--x-token` override | `--x-token` |
| `Agent ID` | auto-generated `agid_<uuidv4-without-dashes>` or `--agent-id` override | `--tag agent_id=<value>` |
| `Agent Name` | `<hostname>_<agent>_<YYYYMMDD>` or `--agent-name` override | `--tag agent_name=<value>` |

The built-in Claude, CodeBuddy, Codex, Cursor, Deep Agents Code, Grok Build, and Kiro adapters accept `--trace-path`, `--metrics-path`, one or more `--header` parameters, one or more `--tag` parameters, `--capture-content`, `--max-chars`, `--enable`, and `--disable`. Values are merged into the existing `gtrace.json`, and unknown fields remain unchanged.

Each built-in adapter writes structured Hook logs to `~/.obs-agent-connector/<agent>/gtrace-hooks.json`. Existing Agent-local configs are read as a compatibility fallback and are migrated into the managed directory when an install or config edit writes new values.

The CLI always uses `--type gtrace`.

## External Plugin Installer Contract

All external OTEL plugins follow one installer contract. The connector downloads
the platform installer and passes only the common telemetry arguments (`latest`,
`--type gtrace`, endpoint, token, tags, and plugin-specific options). The
installer is responsible for resolving its own release archive, verifying its
checksum, installing into the Agent profile, and merging Agent-local runtime
configuration. The connector must not inject `--source`, construct a plugin
archive URL, or write an external plugin's private configuration.

This keeps OSS and GitHub Release delivery interchangeable and makes Unix and
Windows installers equivalent. New external plugins should implement
`install.sh`, `install-release.sh`, and `install-release.ps1` against this
contract and add a regression test for the generated command.

## Runtime Toggle

`enable <agent>` and `disable <agent>` change the plugin runtime switch without reinstalling:

| Agent | Updated JSON path |
| --- | --- |
| `claude` | `enabled` |
| `codebuddy` | `enabled` |
| `codex` | `enabled` |
| `cursor` | `enabled` |
| `dcode` | `enabled` |
| `grok` | `enabled` |
| `kiro` | `enabled` |
| `dsh` | `enabled` |
| `opencode` | `enabled` |
| `openclaw` | `plugins.entries.openclaw-otel-plugin.enabled` |
| `qoder` | `enabled` |
| `workbuddy` | `enabled` |

`hermes` is not included because its runtime config is `~/.hermes/config.yaml`.
