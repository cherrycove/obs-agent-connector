package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisteredPluginNames(t *testing.T) {
	expected := map[string]string{
		"claude":    "claude-otel-plugin",
		"codebuddy": "obs-agent-connector",
		"codex":     "codex-otel-plugin",
		"cursor":    "cursor-otel-plugin",
		"dcode":     "obs-agent-connector",
		"dsh":       "dsh-otel-plugin",
		"hermes":    "hermes-otel-plugin",
		"kiro":      "obs-agent-connector",
		"opencode":  "opencode-otel-plugin",
		"openclaw":  "openclaw-otel-plugin",
		"qoder":     "qoder-otel-plugin",
		"qoder-cn":  "qoder-otel-plugin",
		"workbuddy": "workbuddy-otel-plugin",
	}

	for name, pluginName := range expected {
		definition, ok := definitions[name]
		if !ok {
			t.Fatalf("missing Agent definition %q", name)
		}
		if definition.PluginName != pluginName {
			t.Fatalf("expected %s plugin name %q, got %q", name, pluginName, definition.PluginName)
		}
		assertNoMigrationArtifact(t, definition)
	}
}

func TestSupportedNamesForWindows(t *testing.T) {
	expected := []string{"claude", "codebuddy", "codex", "cursor", "dcode", "dsh", "kiro", "openclaw", "opencode", "qoder", "workbuddy"}
	got := SupportedNames("windows")
	if strings.Join(got, ",") != strings.Join(expected, ",") {
		t.Fatalf("expected Windows supported names %v, got %v", expected, got)
	}
}

func TestSupportedNamesForLinux(t *testing.T) {
	expected := []string{"claude", "codebuddy", "codex", "cursor", "dcode", "dsh", "hermes", "kiro", "openclaw", "opencode", "qoder"}
	got := SupportedNames("linux")
	if strings.Join(got, ",") != strings.Join(expected, ",") {
		t.Fatalf("expected Linux supported names %v, got %v", expected, got)
	}
}

func TestWindowsSupportFlags(t *testing.T) {
	cases := map[string]bool{
		"claude":    true,
		"codebuddy": true,
		"codex":     true,
		"cursor":    true,
		"dcode":     true,
		"dsh":       true,
		"hermes":    false,
		"kiro":      true,
		"opencode":  true,
		"openclaw":  true,
		"qoder":     true,
		"qoder-cn":  true,
		"workbuddy": true,
	}

	for name, expected := range cases {
		definition := definitions[name]
		if got := SupportsPlatform(definition, "windows"); got != expected {
			t.Fatalf("expected %s windows support %t, got %t", name, expected, got)
		}
	}
}

func TestClaudeAndCodexUseBuiltinRuntime(t *testing.T) {
	for _, name := range []string{"claude", "codebuddy", "codex", "dcode"} {
		selected, err := Select(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(selected) != 1 || !selected[0].IsBuiltin() {
			t.Fatalf("unexpected builtin definition for %s: %#v", name, selected)
		}
	}
	if definitions["codex"].ResolveInstall == nil {
		t.Fatal("expected Codex install resolver for automatic hook trust")
	}
}

func TestClaudeBuiltinKeepsLegacyPluginName(t *testing.T) {
	selected, err := Select("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || !selected[0].IsBuiltin() || selected[0].PluginName != "claude-otel-plugin" {
		t.Fatalf("unexpected Claude definition: %#v", selected)
	}
}

func TestCodeBuddyUsesBuiltinConnectorPluginName(t *testing.T) {
	selected, err := Select("codebuddy")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || !selected[0].IsBuiltin() || selected[0].PluginName != "obs-agent-connector" {
		t.Fatalf("unexpected CodeBuddy definition: %#v", selected)
	}
}

func TestDiscoverUsesBuiltinClaudeAndCodexDefinitions(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	candidates := DiscoverCandidatesForOS("linux")
	found := map[string]Candidate{}
	for _, candidate := range candidates {
		found[candidate.Plugin.Name] = candidate
	}
	claude, ok := found["claude"]
	if !ok || !claude.Supported || !claude.Plugin.IsBuiltin() || claude.Plugin.PluginName != "claude-otel-plugin" {
		t.Fatalf("expected builtin claude candidate, got %#v", claude)
	}
	codex, ok := found["codex"]
	if !ok || !codex.Supported || !codex.Plugin.IsBuiltin() || codex.Plugin.PluginName != "codex-otel-plugin" {
		t.Fatalf("expected builtin codex candidate, got %#v", codex)
	}
}

func TestDiscoverCandidatesIncludesCodexOnWindowsWithResolvedBinary(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "codex.exe")
	if err := os.MkdirAll(filepath.Dir(command), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CODEX_BINARY", command)
	t.Setenv("CODEX_CLI_PATH", "")
	t.Setenv("PATH", t.TempDir())

	candidates := DiscoverCandidatesForOS("windows")
	for _, candidate := range candidates {
		if candidate.Plugin.Name != "codex" {
			continue
		}
		if candidate.DetectedCmd != command {
			t.Fatalf("expected resolved windows codex command %q, got %q", command, candidate.DetectedCmd)
		}
		return
	}
	t.Fatal("expected codex to be discoverable on windows from resolved binary path")
}

func TestLinuxSupportFlags(t *testing.T) {
	cases := map[string]bool{
		"claude":    true,
		"codebuddy": true,
		"codex":     true,
		"cursor":    true,
		"dcode":     true,
		"dsh":       true,
		"hermes":    true,
		"kiro":      true,
		"opencode":  true,
		"openclaw":  true,
		"qoder":     true,
		"qoder-cn":  true,
		"workbuddy": false,
	}

	for name, expected := range cases {
		definition := definitions[name]
		if got := SupportsPlatform(definition, "linux"); got != expected {
			t.Fatalf("expected %s linux support %t, got %t", name, expected, got)
		}
	}
}

func TestDiscoverCandidatesIncludesQoderWithoutCommandWhenDataDirExists(t *testing.T) {
	home := t.TempDir()
	previousHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("HOME", previousHome)
	})

	if err := os.MkdirAll(filepath.Join(home, ".qoder"), 0o755); err != nil {
		t.Fatal(err)
	}

	candidates := DiscoverCandidatesForOS("linux")
	for _, candidate := range candidates {
		if candidate.Plugin.Name != "qoder" {
			continue
		}
		if candidate.DetectedCmd != "data-dir" {
			t.Fatalf("expected qoder detect source data-dir, got %q", candidate.DetectedCmd)
		}
		return
	}
	t.Fatal("expected qoder to be discoverable from data dir")
}

func TestDiscoverCandidatesIncludesWorkBuddyWithoutCommandWhenProfileDirExists(t *testing.T) {
	home := t.TempDir()
	previousHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("HOME", previousHome)
	})

	if err := os.MkdirAll(filepath.Join(home, ".workbuddy"), 0o755); err != nil {
		t.Fatal(err)
	}

	candidates := DiscoverCandidatesForOS("darwin")
	for _, candidate := range candidates {
		if candidate.Plugin.Name != "workbuddy" {
			continue
		}
		if candidate.DetectedCmd != "data-dir" {
			t.Fatalf("expected workbuddy detect source data-dir, got %q", candidate.DetectedCmd)
		}
		return
	}
	t.Fatal("expected workbuddy to be discoverable from profile dir")
}

func TestDiscoverCandidatesSkipsWorkBuddyOnLinux(t *testing.T) {
	home := t.TempDir()
	previousHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("HOME", previousHome)
	})

	if err := os.MkdirAll(filepath.Join(home, ".workbuddy"), 0o755); err != nil {
		t.Fatal(err)
	}

	candidates := DiscoverCandidatesForOS("linux")
	for _, candidate := range candidates {
		if candidate.Plugin.Name == "workbuddy" {
			t.Fatal("did not expect workbuddy to be discoverable on linux")
		}
	}
}

func TestDiscoverCandidatesIncludesOpencodeWithoutCommandWhenConfigDirExists(t *testing.T) {
	home := t.TempDir()
	previousHome := os.Getenv("HOME")
	previousPath := os.Getenv("PATH")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("PATH", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("HOME", previousHome)
		_ = os.Setenv("PATH", previousPath)
	})

	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}

	candidates := DiscoverCandidatesForOS("linux")
	for _, candidate := range candidates {
		if candidate.Plugin.Name != "opencode" {
			continue
		}
		if candidate.DetectedCmd != "data-dir" {
			t.Fatalf("expected opencode detect source data-dir, got %q", candidate.DetectedCmd)
		}
		return
	}
	t.Fatal("expected opencode to be discoverable from config dir")
}

func assertNoMigrationArtifact(t *testing.T, definition Definition) {
	t.Helper()
	values := append([]string{definition.PluginName}, definition.Markers...)
	values = append(values, definition.ConfigFiles...)
	values = append(values, definition.RemovePaths...)
	for _, command := range definition.RemoveCmds {
		values = append(values, command...)
	}
	for _, value := range values {
		if strings.Contains(value, "Definition") {
			t.Fatalf("Agent %s contains invalid migrated value %q", definition.Name, value)
		}
	}
}
