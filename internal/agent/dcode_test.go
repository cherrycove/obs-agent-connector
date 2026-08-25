package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDcodeForInstallRequiresHomeOrCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DCODE_BINARY", "")
	t.Setenv("DEEPAGENTS_CODE_BINARY", "")
	t.Setenv("DCODE_CLI_PATH", "")

	if _, err := ResolveForInstall([]Definition{definitions["dcode"]}); err == nil {
		t.Fatal("expected dcode install resolution to fail without data dir or command")
	}
	if err := os.MkdirAll(filepath.Join(home, ".deepagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveForInstall([]Definition{definitions["dcode"]})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved[0].ConfigFiles; len(got) != 2 || got[0] != "~/.obs-agent-connector/dcode/gtrace.json" || got[1] != "~/.deepagents/gtrace.json" {
		t.Fatalf("unexpected dcode config files: %#v", got)
	}
}

func TestDiscoverCandidatesIncludesDcodeCommand(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	t.Setenv("DCODE_BINARY", "")
	t.Setenv("DEEPAGENTS_CODE_BINARY", "")
	t.Setenv("DCODE_CLI_PATH", "")
	command := filepath.Join(binDir, "dcode")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range DiscoverCandidatesForOS("linux") {
		if candidate.Plugin.Name != "dcode" {
			continue
		}
		if candidate.DetectedCmd != command || !candidate.Plugin.IsBuiltin() {
			t.Fatalf("unexpected dcode candidate: %#v", candidate)
		}
		return
	}
	t.Fatal("expected dcode to be discoverable")
}

func TestDcodeInstalledMarkerUsesManagedHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".obs-agent-connector", "dcode", "gtrace.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin := definitions["dcode"]
	if path, installed := InstalledMarker(plugin); installed || path != "" {
		t.Fatalf("config without a managed Hook must not count as installed: path=%q installed=%t", path, installed)
	}

	hooksPath := filepath.Join(home, ".deepagents", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	unrelated := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo unrelated"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, installed := InstalledMarker(plugin); installed || path != "" {
		t.Fatalf("unrelated Hook must not count as installed: path=%q installed=%t", path, installed)
	}

	managed := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/tmp/obs-agent-connector hook dcode Stop"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(managed), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, installed := InstalledMarker(plugin); !installed || path != hooksPath {
		t.Fatalf("managed dcode Hook was not detected: path=%q installed=%t", path, installed)
	}
}
