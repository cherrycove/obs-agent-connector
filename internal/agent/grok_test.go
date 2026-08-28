package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGrokVersion(t *testing.T) {
	cases := map[string]string{
		"1.0.5":                     "1.0.5",
		"grok 1.0.5 (5115b46)":      "1.0.5",
		"grok 1.0.10 (77cd7eb)":     "1.0.10",
		"Grok Build v1.2.3-alpha.1": "1.2.3-alpha.1",
		"grok nightly":              "",
		"1.0":                       "",
	}
	for input, expected := range cases {
		got, ok := parseGrokVersion(input)
		if got != expected || ok != (expected != "") {
			t.Fatalf("parseGrokVersion(%q) = %q, %t; want %q", input, got, ok, expected)
		}
	}
}

func TestGrokVersionMinimum(t *testing.T) {
	cases := map[string]bool{
		"1.0.4":         false,
		"1.0.5-alpha.1": false,
		"1.0.5":         true,
		"1.0.5+build.1": true,
		"1.0.10":        true,
		"1.1.0-alpha.1": true,
		"2.0.0":         true,
	}
	for version, expected := range cases {
		if got := grokVersionAtLeast(version, MinimumGrokVersion); got != expected {
			t.Fatalf("grokVersionAtLeast(%q) = %t, want %t", version, got, expected)
		}
	}
}

func TestResolveGrokForInstallChecksKnownVersion(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	t.Setenv("GROK_BINARY", "")
	t.Setenv("GROK_CLI_PATH", "")
	command := filepath.Join(binDir, "grok")
	writeGrokCommand(t, command, "grok 1.0.4 (synthetic)")

	if _, err := resolveGrokForInstall(definitions["grok"]); err == nil || !strings.Contains(err.Error(), MinimumGrokVersion) {
		t.Fatalf("expected minimum version error, got %v", err)
	}
	writeGrokCommand(t, command, "grok 1.0.5 (synthetic)")
	resolved, err := resolveGrokForInstall(definitions["grok"])
	if err != nil {
		t.Fatal(err)
	}
	if resolved.AgentCommand != command {
		t.Fatalf("resolved command = %q, want %q", resolved.AgentCommand, command)
	}
}

func TestResolveGrokForInstallAllowsUnknownVersion(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	t.Setenv("GROK_BINARY", "")
	t.Setenv("GROK_CLI_PATH", "")
	command := filepath.Join(binDir, "grok")
	writeGrokCommand(t, command, "grok nightly")
	if _, err := resolveGrokForInstall(definitions["grok"]); err != nil {
		t.Fatalf("unknown Grok version should be allowed: %v", err)
	}
}

func TestDetectGrokVersionResolvesConfiguredDefaultCommand(t *testing.T) {
	command := filepath.Join(t.TempDir(), "grok")
	writeGrokCommand(t, command, "grok 1.0.5 (synthetic)")
	t.Setenv("GROK_BINARY", command)
	t.Setenv("GROK_CLI_PATH", "")
	if version, ok := DetectGrokVersion("grok"); !ok || version != "1.0.5" {
		t.Fatalf("DetectGrokVersion(grok) = %q, %t", version, ok)
	}
}

func TestResolveGrokRequiresCommandOrDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GROK_BINARY", "")
	t.Setenv("GROK_CLI_PATH", "")
	if _, err := resolveGrokForInstall(definitions["grok"]); err == nil {
		t.Fatal("expected Grok install resolution to reject a missing product")
	}
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveGrokForInstall(definitions["grok"]); err != nil {
		t.Fatalf("expected Grok data directory fallback: %v", err)
	}
}

func TestDiscoverGrokByCommandOrDataDirectory(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	t.Setenv("GROK_BINARY", "")
	t.Setenv("GROK_CLI_PATH", "")
	command := filepath.Join(binDir, "grok")
	writeGrokCommand(t, command, "grok 1.0.5")
	resolved, ok := resolveGrokForDiscovery(definitions["grok"])
	if !ok || resolved.AgentCommand != command {
		t.Fatalf("unexpected Grok command discovery: ok=%t definition=%#v", ok, resolved)
	}

	if err := os.Remove(command); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, ok = resolveGrokForDiscovery(definitions["grok"])
	if !ok || resolved.AgentCommand != "grok" {
		t.Fatalf("unexpected Grok data directory discovery: ok=%t definition=%#v", ok, resolved)
	}
}

func TestGrokInstalledMarkerRequiresManagedHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hookPath := filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo unrelated"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, installed := InstalledMarker(definitions["grok"]); installed || path != "" {
		t.Fatalf("unrelated Hook must not count as installed: path=%q installed=%t", path, installed)
	}
	if err := os.WriteFile(hookPath, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/tmp/obs-agent-connector hook grok Stop"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, installed := InstalledMarker(definitions["grok"]); !installed || path != hookPath {
		t.Fatalf("managed Grok Hook was not detected: path=%q installed=%t", path, installed)
	}
}

func writeGrokCommand(t *testing.T, path, version string) {
	t.Helper()
	body := "#!/bin/sh\nprintf '%s\\n' '" + version + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
