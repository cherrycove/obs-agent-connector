package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovePathLineFromContent(t *testing.T) {
	original := "export PATH=\"/a/bin:$PATH\"\nexport PATH=\"/b/bin:$PATH\"\n"
	updated, changed := removePathLineFromContent(original, `export PATH="/a/bin:$PATH"`)
	if !changed {
		t.Fatal("expected content to change")
	}
	if strings.Contains(updated, `/a/bin`) {
		t.Fatalf("expected target PATH line to be removed, got %q", updated)
	}
	if !strings.Contains(updated, `/b/bin`) {
		t.Fatalf("expected unrelated PATH line to be kept, got %q", updated)
	}
}

func TestRemovePathEntry(t *testing.T) {
	updated, changed := removePathEntry(`C:\A;C:\B;C:\C`, `C:\B`, ";")
	if !changed {
		t.Fatal("expected path list to change")
	}
	if updated != `C:\A;C:\C` {
		t.Fatalf("unexpected updated PATH %q", updated)
	}
}

func TestUninstallDryRun(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	executablePath := filepath.Join(home, ".local", "bin", "obs-agent-connector")
	if err := os.MkdirAll(filepath.Dir(executablePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executablePath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(home, ".obs-agent-connector")
	configPath := filepath.Join(configDir, "config.json")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	zshrcPath := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(zshrcPath, []byte("export PATH=\""+filepath.Dir(executablePath)+":$PATH\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previousExecutable := currentExecutable
	previousEvalSymlinks := currentEvalSymlinks
	previousGOOS := currentGOOS
	currentExecutable = func() (string, error) { return executablePath, nil }
	currentEvalSymlinks = func(path string) (string, error) { return path, nil }
	currentGOOS = "linux"
	t.Cleanup(func() {
		currentExecutable = previousExecutable
		currentEvalSymlinks = previousEvalSymlinks
		currentGOOS = previousGOOS
	})

	output := captureStdout(t, func() {
		if err := uninstallConnector([]string{"--dry-run"}); err != nil {
			t.Fatal(err)
		}
	})

	for _, expected := range []string{
		"Uninstall plan:",
		"Binary         : " + executablePath,
		"Built-in Agents: remove claude, codebuddy, codex, cursor, dcode, grok, and kiro; remove managed config, logs, and state",
		"Config         : remove " + configPath,
		"Shell PATH     : remove managed entry from " + zshrcPath,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected output to contain %q, got:\n%s", expected, output)
		}
	}
	if _, err := os.Stat(executablePath); err != nil {
		t.Fatalf("expected binary to remain during dry-run: %v", err)
	}
}

func TestUninstallRemovesAllBuiltInAdaptersAndManagedFiles(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("PATH", t.TempDir())

	executablePath := filepath.Join(home, ".local", "bin", "obs-agent-connector")
	if err := os.MkdirAll(filepath.Dir(executablePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executablePath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	configRoot := filepath.Join(home, ".obs-agent-connector")
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "config.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, adapter := range []string{"claude", "codebuddy", "codex", "cursor", "dcode", "grok", "kiro"} {
		dir := filepath.Join(configRoot, adapter)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gtrace.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gtrace-hooks.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	hookFiles := map[string]string{
		filepath.Join(home, ".claude", "settings.json"):                   `{"hooks":{"Stop":[{"hooks":[{"command":"/tmp/obs-agent-connector hook claude"}]}],"SessionEnd":[]}}`,
		filepath.Join(home, ".codebuddy", "settings.json"):                `{"hooks":{"Stop":[{"hooks":[{"command":"/tmp/obs-agent-connector hook codebuddy"}]}],"SessionEnd":[]}}`,
		filepath.Join(home, ".codex", "hooks.json"):                       `{"hooks":{"Stop":[{"hooks":[{"command":"/tmp/obs-agent-connector hook codex"}]}]}}`,
		filepath.Join(home, ".cursor", "hooks.json"):                      `{"version":1,"hooks":{"stop":[{"command":"/tmp/obs-agent-connector hook cursor stop"}]}}`,
		filepath.Join(home, ".deepagents", "hooks.json"):                  `{"hooks":{"Stop":[{"hooks":[{"command":"/tmp/obs-agent-connector hook dcode Stop"}]}]}}`,
		filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json"): `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/tmp/obs-agent-connector hook grok Stop"}]}]}}`,
		filepath.Join(home, ".kiro", "hooks", "obs-agent-connector.json"): `{"version":"v1","hooks":[{"name":"managed","trigger":"Stop","action":{"type":"command","command":"/tmp/obs-agent-connector hook kiro Stop"}}]}`,
	}
	for path, body := range hookFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	previousExecutable := currentExecutable
	previousEvalSymlinks := currentEvalSymlinks
	previousGOOS := currentGOOS
	currentExecutable = func() (string, error) { return executablePath, nil }
	currentEvalSymlinks = func(path string) (string, error) { return path, nil }
	currentGOOS = "linux"
	t.Cleanup(func() {
		currentExecutable = previousExecutable
		currentEvalSymlinks = previousEvalSymlinks
		currentGOOS = previousGOOS
	})

	if err := uninstallConnector([]string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(executablePath); !os.IsNotExist(err) {
		t.Fatalf("connector binary remains: %v", err)
	}
	if _, err := os.Stat(configRoot); !os.IsNotExist(err) {
		t.Fatalf("connector config root remains: %v", err)
	}
	for path := range hookFiles {
		body, err := os.ReadFile(path)
		if os.IsNotExist(err) && (strings.Contains(path, filepath.Join(".grok", "hooks")) || strings.Contains(path, filepath.Join(".kiro", "hooks"))) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "obs-agent-connector") {
			t.Fatalf("managed Hook remains in %s: %s", path, body)
		}
	}
}

func TestUninstallKeepConfigPreservesManagedConfigAndRemovesHookLog(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("PATH", t.TempDir())

	executablePath := filepath.Join(home, ".local", "bin", "obs-agent-connector")
	managedDir := filepath.Join(home, ".obs-agent-connector", "codex")
	globalConfigPath := filepath.Join(home, ".obs-agent-connector", "config.json")
	managedConfigPath := filepath.Join(managedDir, "gtrace.json")
	managedLogPath := filepath.Join(managedDir, "gtrace-hooks.json")
	for _, path := range []string{executablePath, globalConfigPath, managedConfigPath, managedLogPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	previousExecutable := currentExecutable
	previousEvalSymlinks := currentEvalSymlinks
	previousGOOS := currentGOOS
	currentExecutable = func() (string, error) { return executablePath, nil }
	currentEvalSymlinks = func(path string) (string, error) { return path, nil }
	currentGOOS = "linux"
	t.Cleanup(func() {
		currentExecutable = previousExecutable
		currentEvalSymlinks = previousEvalSymlinks
		currentGOOS = previousGOOS
	})

	if err := uninstallConnector([]string{"--yes", "--keep-config"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{globalConfigPath, managedConfigPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("kept config is missing at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(managedLogPath); !os.IsNotExist(err) {
		t.Fatalf("managed Hook log remains: %v", err)
	}
}
