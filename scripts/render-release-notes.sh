#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUTPUT_FILE="${1:-${ROOT_DIR}/dist/RELEASE_NOTES.md}"
VERSION="${VERSION:-dev}"

mkdir -p "$(dirname "${OUTPUT_FILE}")"

append_line() {
  line="$1"
  if [ -z "${NOTES:-}" ]; then
    NOTES="$line"
    return
  fi
  case "
${NOTES}
" in
    *"
${line}
"*) ;;
    *) NOTES="${NOTES}
${line}" ;;
  esac
}

previous_tag() {
  case "${VERSION}" in
    *-*) ;;
    *)
      git -C "${ROOT_DIR}" tag --sort=version:refname \
        | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' \
        | grep -Fxv "${VERSION}" \
        | tail -n 1
      return
      ;;
  esac

  git -C "${ROOT_DIR}" tag --sort=version:refname \
    | grep -Fxv "${VERSION}" \
    | tail -n 1
}

render_changes() {
  previous="$(previous_tag || true)"
  NOTES=""

  if [ -n "${previous}" ]; then
    changed_files="$(git -C "${ROOT_DIR}" diff --name-only "${previous}..${VERSION}")"
    kiro_release=false
    dcode_release=false
    grok_release=false

    if printf '%s\n' "${changed_files}" | grep -Eq '(^|/)grok([^/]*|/.*)$'; then
      if git -C "${ROOT_DIR}" cat-file -e "${previous}:internal/adapters/grok" 2>/dev/null; then
        append_line "- Improved Grok Build telemetry, including Windows Hook execution, transcript replay, per-LLM token attribution, duplicate/background turn suppression, and display-safe assistant spans."
      else
        append_line "- Added built-in Grok Build 1.0.5+ discovery, cross-platform Hook lifecycle management, durable transcript replay, and OTLP traces and metrics for LLM, tool, and assistant operations."
        append_line "- Added Grok collection reliability and compatibility fixes, including Windows PowerShell/cmd Hook execution, per-LLM token attribution, duplicate/background turn suppression, and display-safe assistant spans."
      fi
      grok_release=true
    fi

    if printf '%s\n' "${changed_files}" | grep -Eq '^internal/agent/dsh(_test)?\.go$'; then
      append_line "- Fixed dsh plugin installation by requiring an available dsh CLI and detecting cached CLI installations."
    fi

    if printf '%s\n' "${changed_files}" | grep -Eq '(^|/)dcode([^/]*|/.*)$'; then
      if git -C "${ROOT_DIR}" cat-file -e "${previous}:internal/adapters/dcode" 2>/dev/null; then
        append_line "- Fixed Dcode installed-state detection so list, status, and discover recognize connector-managed Hooks."
      else
        append_line "- Added built-in Deep Agents Code discovery, Hooks v2 lifecycle management, transcript replay telemetry, and persistent signal-level retry."
      fi
      dcode_release=true
    fi

    if printf '%s\n' "${changed_files}" | grep -Eq '(^|/)kiro([^/]*|/.*)$'; then
      if git -C "${ROOT_DIR}" cat-file -e "${previous}:internal/adapters/kiro" 2>/dev/null; then
        if git -C "${ROOT_DIR}" diff "${previous}..${VERSION}" -- internal/core/semantic/semantic.go | grep -F 'removeUsageAttrs' >/dev/null; then
          append_line "- Removed Token and Credit fields from non-Kiro invoke_agent spans while preserving Kiro aggregate billing credit on invoke_agent."
        elif git -C "${ROOT_DIR}" diff "${previous}..${VERSION}" -- internal/adapters/kiro | grep -F 'gen_ai.usage.credit' >/dev/null; then
          append_line "- Added Kiro billing credit telemetry on invoke_agent through the gen_ai.usage.credit attribute without fabricating token usage."
        else
          append_line "- Fixed Kiro CLI 2.19.1 telemetry replay with exact session matching, modern workspace-bucketed session support, and legacy v3 compatibility."
        fi
      else
        append_line "- Added built-in Kiro CLI discovery, lifecycle management, hooks, session replay telemetry, and persistent retry."
      fi
      kiro_release=true
    fi

    if [ "${kiro_release}" != "true" ] && [ "${dcode_release}" != "true" ] && [ "${grok_release}" != "true" ] && printf '%s\n' "${changed_files}" | grep -Eq '^(cmd/|internal/app/|internal/agent/|main\.go|main_test\.go)'; then
      append_line "- Reorganized the CLI into cmd/ and internal/ packages, with separate app and Agent modules."
    fi

    if [ "${kiro_release}" != "true" ] && [ "${dcode_release}" != "true" ] && [ "${grok_release}" != "true" ] && printf '%s\n' "${changed_files}" | grep -Eq '^(internal/app/command_discover\.go|internal/app/command_install\.go|internal/app/command_update\.go|internal/app/installer\.go|internal/agent/(definition|registry|codex|openclaw|qoder)\.go)'; then
      append_line "- Improved installation and update flows, including Windows-specific plugin installer routing for supported Agents."
    fi

    if printf '%s\n' "${changed_files}" | grep -Eq '^(internal/app/command_toggle\.go|internal/app/version\.go|internal/app/version_test\.go)'; then
      append_line "- Added runtime plugin enable/disable commands and direct self-update support with version -u."
    fi

    if printf '%s\n' "${changed_files}" | grep -Eq '^(README\.md|docs/|AGENTS\.md|\.github/workflows/|scripts/build-release\.sh)'; then
      append_line "- Updated documentation and release metadata."
    fi

    if [ -n "${NOTES}" ]; then
      printf '%s\n' "${NOTES}"
      return
    fi

    git -C "${ROOT_DIR}" log --format='- %s' "${previous}..${VERSION}" | head -n 5
    return
  fi

  git -C "${ROOT_DIR}" log --format='- %s' "${VERSION}" -n 5
}

CHANGES="$(render_changes)"
if [ -z "${CHANGES}" ]; then
  CHANGES="- Packaging update"
fi

printf '%s\n' "${CHANGES}" > "${OUTPUT_FILE}"
