package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GuanceCloud/obs-agent-connector/internal/agent"
)

func TestGrokInstallUsesBuiltin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GROK_BINARY", "")
	t.Setenv("GROK_CLI_PATH", "")
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".obs-agent-connector", "config.json")
	t.Setenv("OBS_AGENT_CONNECTOR_CONFIG", configPath)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"endpoint":"https://example.com","x_token":"synthetic-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	output := captureStdout(t, func() {
		if err := install([]string{"grok", "--dry-run", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(output, "grok (built into obs-agent-connector)") || strings.Contains(output, "grok-otel-plugin") {
		t.Fatalf("unexpected Grok install plan: %s", output)
	}
}

func TestGrokBuiltinInstallWritesHookAndReloadNote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GROK_BINARY", "")
	t.Setenv("GROK_CLI_PATH", "")
	executable := filepath.Join(home, ".local", "bin", "obs-agent-connector")
	originalExecutable := currentExecutable
	currentExecutable = func() (string, error) { return executable, nil }
	t.Cleanup(func() { currentExecutable = originalExecutable })

	output := captureStdout(t, func() {
		if err := installBuiltinAdapter(agent.Get("grok"), installInput{}, true); err != nil {
			t.Fatal(err)
		}
	})
	hookPath := filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json")
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), executable) || !strings.Contains(string(body), "hook grok Stop") {
		t.Fatalf("managed Grok Hooks were not written: %s", body)
	}
	if !strings.Contains(output, "press l in the Hooks tab") {
		t.Fatalf("Grok install did not report reload guidance: %s", output)
	}
	if !strings.Contains(output, "Could not determine the Grok Build version") || !strings.Contains(output, agent.MinimumGrokVersion) {
		t.Fatalf("Grok install did not report the unknown version warning: %s", output)
	}
}

func TestGrokBuiltinInstallRejectsKnownUnsupportedVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	command := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '1.0.4\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(home, ".local", "bin", "obs-agent-connector")
	originalExecutable := currentExecutable
	currentExecutable = func() (string, error) { return executable, nil }
	t.Cleanup(func() { currentExecutable = originalExecutable })
	plugin := agent.Get("grok")
	plugin.AgentCommand = command

	err := installBuiltinAdapter(plugin, installInput{}, true)
	if err == nil || !strings.Contains(err.Error(), agent.MinimumGrokVersion) {
		t.Fatalf("expected unsupported Grok version error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json")); !os.IsNotExist(statErr) {
		t.Fatalf("unsupported Grok version wrote Hooks: %v", statErr)
	}
}

func TestGrokBuiltinInstallPersistsDetectedAgentVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	command := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf 'grok 1.0.5 (synthetic)\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(home, ".local", "bin", "obs-agent-connector")
	originalExecutable := currentExecutable
	currentExecutable = func() (string, error) { return executable, nil }
	t.Cleanup(func() { currentExecutable = originalExecutable })
	plugin := agent.Get("grok")
	plugin.AgentCommand = command

	if err := installBuiltinAdapter(plugin, installInput{}, false); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(home, ".obs-agent-connector", "grok", "gtrace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"agent_version": "1.0.5"`) {
		t.Fatalf("detected Grok version was not persisted as a resource attribute: %s", body)
	}
}
