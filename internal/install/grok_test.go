package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallGrokWritesGlobalHooksAndManagedConfig(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksFile := filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json")
	if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"future":{"keep":true},"hooks":{"Stop":[{"matcher":"other","hooks":[{"type":"command","command":"echo keep"},{"type":"command","command":"/old/obs-agent-connector hook grok Stop"}]}],"CustomEvent":[{"hooks":[{"type":"command","command":"echo future"}]}]}}`
	if err := os.WriteFile(hooksFile, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	result, err := InstallGrok(GrokOptions{
		Home: home, SourceExecutable: source, DestinationExecutable: source,
		Endpoint: "https://example.invalid", InstallType: "gtrace", XToken: "synthetic-token", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigFile != filepath.Join(home, ".obs-agent-connector", "grok", "gtrace.json") || !result.Configured {
		t.Fatalf("unexpected install result: %#v", result)
	}
	body, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "echo keep") || !strings.Contains(text, "echo future") || strings.Contains(text, "/old/obs-agent-connector") {
		t.Fatalf("Grok Hook reconciliation did not preserve unrelated handlers: %s", body)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if value["future"].(map[string]any)["keep"] != true {
		t.Fatalf("unknown top-level values were not preserved: %#v", value)
	}
	hooks := value["hooks"].(map[string]any)
	for _, event := range grokHookEvents {
		groups, ok := hooks[event].([]any)
		if !ok || len(groups) == 0 {
			t.Fatalf("missing %s Grok Hook: %s", event, body)
		}
		found := false
		for _, rawGroup := range groups {
			group, _ := rawGroup.(map[string]any)
			for _, rawHandler := range group["hooks"].([]any) {
				handler := rawHandler.(map[string]any)
				if strings.Contains(handler["command"].(string), "hook grok "+event) {
					found = handler["timeout"] == float64(5)
					if event == "Notification" && group["matcher"] != "idle_prompt" {
						t.Fatalf("Notification Hook is missing idle_prompt matcher: %#v", group)
					}
				}
			}
		}
		if !found {
			t.Fatalf("missing managed %s Grok Hook with 5 second timeout: %s", event, body)
		}
	}
	configBody, err := os.ReadFile(result.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configBody), `"enabled": true`) {
		t.Fatalf("Grok config does not contain top-level enabled: %s", configBody)
	}
}

func TestInstallGrokNoConfigPreservesRuntimeConfig(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	configFile := filepath.Join(home, ".obs-agent-connector", "grok", "gtrace.json")
	for _, path := range []string{source, configFile} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"enabled\":false,\"unknown\":true}\n")
	if err := os.WriteFile(configFile, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallGrok(GrokOptions{Home: home, SourceExecutable: source, DestinationExecutable: source, NoConfig: true}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatalf("--no-config changed Grok config: %s", body)
	}
}

func TestInstallGrokRejectsMalformedHooksWithoutOverwriting(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	hooksFile := filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json")
	if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"hooks":{"Stop":{"unexpected":true}}}`)
	if err := os.WriteFile(hooksFile, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallGrok(GrokOptions{Home: home, SourceExecutable: source, DestinationExecutable: source}); err == nil {
		t.Fatal("expected malformed Grok Hook configuration to fail")
	}
	body, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatalf("malformed Grok Hook file was overwritten: %s", body)
	}
}

func TestGrokHookCommandUsesSelectedPlatformShell(t *testing.T) {
	executable := `C:\Program Files\Guance\obs-agent-connector.exe`
	tests := []struct {
		name          string
		goos          string
		requested     string
		bashAvailable bool
		want          string
	}{
		{name: "unix", goos: "linux", want: `"C:\Program Files\Guance\obs-agent-connector.exe" hook grok Stop`},
		{name: "windows default", goos: "windows", want: `& 'C:\Program Files\Guance\obs-agent-connector.exe' hook grok Stop`},
		{name: "PowerShell 7", goos: "windows", requested: "pwsh", want: `& 'C:\Program Files\Guance\obs-agent-connector.exe' hook grok Stop`},
		{name: "Windows PowerShell", goos: "windows", requested: "powershell", want: `& 'C:\Program Files\Guance\obs-agent-connector.exe' hook grok Stop`},
		{name: "cmd override", goos: "windows", requested: "cmd", want: `"C:\Program Files\Guance\obs-agent-connector.exe" hook grok Stop`},
		{name: "Git Bash override", goos: "windows", requested: "bash", bashAvailable: true, want: `'C:/Program Files/Guance/obs-agent-connector.exe' hook grok Stop`},
		{name: "missing Git Bash falls back", goos: "windows", requested: "bash", want: `& 'C:\Program Files\Guance\obs-agent-connector.exe' hook grok Stop`},
		{name: "unknown override follows default", goos: "windows", requested: "fish", want: `& 'C:\Program Files\Guance\obs-agent-connector.exe' hook grok Stop`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := grokHookCommandForPlatform(executable, "Stop", test.goos, test.requested, test.bashAvailable)
			if got != test.want {
				t.Fatalf("Grok Hook command = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGrokHookCommandEscapesShellSpecificPaths(t *testing.T) {
	powerShell := grokHookCommandForPlatform(`C:\Users\O'Brien\connector.exe`, "Stop", "windows", "pwsh", false)
	if powerShell != `& 'C:\Users\O''Brien\connector.exe' hook grok Stop` {
		t.Fatalf("PowerShell Hook command = %q", powerShell)
	}
	gitBash := grokHookCommandForPlatform(`C:\Users\O'Brien\connector.exe`, "Stop", "windows", "bash", true)
	if gitBash != `'C:/Users/O'"'"'Brien/connector.exe' hook grok Stop` {
		t.Fatalf("Git Bash Hook command = %q", gitBash)
	}
}

func TestRemoveGrokPreservesUnrelatedHooksAndPurgesManagedFiles(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallGrok(GrokOptions{Home: home, SourceExecutable: source, DestinationExecutable: source}); err != nil {
		t.Fatal(err)
	}
	hooksFile := filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json")
	value, err := readJSONObject(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	hooks := value["hooks"].(map[string]any)
	stopGroups := hooks["Stop"].([]any)
	stopGroups = append(stopGroups, map[string]any{"matcher": "other", "hooks": []any{map[string]any{"type": "command", "command": "echo keep"}}})
	hooks["Stop"] = stopGroups
	value["future"] = true
	if err := writeJSONAtomic(hooksFile, value); err != nil {
		t.Fatal(err)
	}
	managedFile := filepath.Join(home, ".obs-agent-connector", "grok", "state", "queued.json")
	if err := os.MkdirAll(filepath.Dir(managedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := RemoveAdapter("grok", home, RemoveOptions{PurgeManaged: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.HookRemoved || !result.ManagedFilesRemoved || !result.ConfigRemoved {
		t.Fatalf("unexpected Grok remove result: %#v", result)
	}
	body, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "echo keep") || strings.Contains(string(body), "hook grok") || !strings.Contains(string(body), `"future": true`) {
		t.Fatalf("unexpected remaining Grok Hooks: %s", body)
	}
	if _, err := os.Stat(filepath.Join(home, ".obs-agent-connector", "grok")); !os.IsNotExist(err) {
		t.Fatalf("managed Grok directory was not removed: %v", err)
	}
}
