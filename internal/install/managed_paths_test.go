package install

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuiltInInstallersUseManagedConfigPaths(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "source", "obs-agent-connector")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	enabled := true

	tests := []struct {
		name    string
		install func() (string, error)
	}{
		{name: "codex", install: func() (string, error) {
			result, err := InstallCodex(CodexOptions{Home: home, SourceExecutable: source, Endpoint: "https://example.invalid", Enabled: &enabled, SkipTrust: true})
			return result.ConfigFile, err
		}},
		{name: "claude", install: func() (string, error) {
			result, err := InstallClaude(ClaudeOptions{Home: home, SourceExecutable: source, Endpoint: "https://example.invalid", Enabled: &enabled})
			return result.ConfigFile, err
		}},
		{name: "cursor", install: func() (string, error) {
			result, err := InstallCursor(CursorOptions{Home: home, SourceExecutable: source, Endpoint: "https://example.invalid", Enabled: &enabled})
			return result.ConfigFile, err
		}},
		{name: "dcode", install: func() (string, error) {
			result, err := InstallDcode(DcodeOptions{Home: home, SourceExecutable: source, Endpoint: "https://example.invalid", Enabled: &enabled})
			return result.ConfigFile, err
		}},
		{name: "codebuddy", install: func() (string, error) {
			result, err := InstallCodeBuddy(CodeBuddyOptions{Home: home, SourceExecutable: source, Endpoint: "https://example.invalid", Enabled: &enabled})
			return result.ConfigFile, err
		}},
		{name: "grok", install: func() (string, error) {
			result, err := InstallGrok(GrokOptions{Home: home, SourceExecutable: source, Endpoint: "https://example.invalid", Enabled: &enabled})
			return result.ConfigFile, err
		}},
		{name: "kiro", install: func() (string, error) {
			result, err := InstallKiro(KiroOptions{Home: home, SourceExecutable: source, Endpoint: "https://example.invalid", Enabled: &enabled})
			return result.ConfigFile, err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configFile, err := test.install()
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(home, ".obs-agent-connector", test.name, "gtrace.json")
			if configFile != want {
				t.Fatalf("config file = %q, want %q", configFile, want)
			}
			if _, err := os.Stat(want); err != nil {
				t.Fatalf("managed config was not generated: %v", err)
			}
		})
	}
}

func TestBuiltInPurgeRemovesManagedAndLegacyFiles(t *testing.T) {
	legacyConfig := map[string]string{
		"codex":     filepath.Join(".codex", "gtrace.json"),
		"claude":    filepath.Join(".claude", "gtrace.json"),
		"cursor":    filepath.Join(".cursor", "gtrace.json"),
		"dcode":     filepath.Join(".deepagents", "gtrace.json"),
		"codebuddy": filepath.Join(".codebuddy", "gtrace.json"),
		"kiro":      filepath.Join(".kiro", "gtrace.json"),
	}
	for agent, legacyRelativePath := range legacyConfig {
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			managedConfig := filepath.Join(home, ".obs-agent-connector", agent, "gtrace.json")
			managedLog := filepath.Join(home, ".obs-agent-connector", agent, "gtrace-hooks.json")
			legacyPath := filepath.Join(home, legacyRelativePath)
			for _, path := range []string{managedConfig, managedLog, legacyPath} {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := RemoveAdapter(agent, home, RemoveOptions{PurgeConfig: true, PurgeState: true})
			if err != nil {
				t.Fatal(err)
			}
			if !result.ConfigRemoved || !result.StatePurged {
				t.Fatalf("purge result = %#v", result)
			}
			for _, path := range []string{managedConfig, managedLog, legacyPath} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("purged file remains at %s: %v", path, err)
				}
			}
		})
	}
}

func TestBuiltInManagedPurgePreservesLegacyAgentConfig(t *testing.T) {
	home := t.TempDir()
	managedDir := filepath.Join(home, ".obs-agent-connector", "codex")
	managedConfig := filepath.Join(managedDir, "gtrace.json")
	managedLog := filepath.Join(managedDir, "gtrace-hooks.json")
	legacyConfig := filepath.Join(home, ".codex", "gtrace.json")
	for _, path := range []string{managedConfig, managedLog, legacyConfig} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := RemoveAdapter("codex", home, RemoveOptions{PurgeManaged: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ConfigRemoved || !result.ManagedFilesRemoved {
		t.Fatalf("managed purge result = %#v", result)
	}
	if _, err := os.Stat(managedDir); !os.IsNotExist(err) {
		t.Fatalf("managed directory remains: %v", err)
	}
	if _, err := os.Stat(legacyConfig); err != nil {
		t.Fatalf("legacy Agent config must be preserved: %v", err)
	}
}

func TestInstallCodexWritesManagedConfigBeforeTrust(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalTrust := trustCodexHook
	defer func() { trustCodexHook = originalTrust }()
	trustCalled := false
	trustCodexHook = func(_ string, gotHome string, _ time.Duration) error {
		trustCalled = true
		path := filepath.Join(gotHome, ".obs-agent-connector", "codex", "gtrace.json")
		current, exists, err := ReadRuntimeConfig(path)
		if err != nil {
			return err
		}
		if !exists || current["enabled"] != true || current["endpoint"] != "https://managed.example" {
			return fmt.Errorf("managed config was not ready before trust: %#v", current)
		}
		return nil
	}
	enabled := true
	result, err := InstallCodex(CodexOptions{
		Home: home, SourceExecutable: source, DestinationExecutable: source,
		CodexCommand: "codex", Endpoint: "https://managed.example", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trustCalled || !result.Configured {
		t.Fatalf("unexpected install result: trustCalled=%t result=%#v", trustCalled, result)
	}
}
