package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallDcodeWritesV2HooksAndManagedConfig(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksFile := filepath.Join(home, ".deepagents", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo keep"},{"type":"command","command":"/old/obs-agent-connector hook dcode Stop"}]}]}}`
	if err := os.WriteFile(hooksFile, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	result, err := InstallDcode(DcodeOptions{
		Home: home, SourceExecutable: source, DestinationExecutable: source,
		Endpoint: "https://example.invalid", InstallType: "gtrace", XToken: "agent_test", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigFile != filepath.Join(home, ".obs-agent-connector", "dcode", "gtrace.json") || !result.Configured {
		t.Fatalf("unexpected install result: %#v", result)
	}
	body, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "echo keep") || strings.Contains(string(body), "/old/obs-agent-connector") {
		t.Fatalf("dcode Hook reconciliation did not preserve unrelated handlers: %s", body)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	hooks := value["hooks"].(map[string]any)
	for _, event := range dcodeHookEvents {
		groups, ok := hooks[event].([]any)
		if !ok || len(groups) == 0 || !strings.Contains(string(body), "hook dcode "+event) {
			t.Fatalf("missing %s dcode Hook: %s", event, body)
		}
	}
}

func TestInstallDcodeNoConfigPreservesRuntimeConfig(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	configFile := filepath.Join(home, ".obs-agent-connector", "dcode", "gtrace.json")
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
	if _, err := InstallDcode(DcodeOptions{Home: home, SourceExecutable: source, DestinationExecutable: source, NoConfig: true}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatalf("--no-config changed dcode config: %s", body)
	}
}

func TestRemoveDcodePreservesUnrelatedHookHandlers(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "obs-agent-connector")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallDcode(DcodeOptions{Home: home, SourceExecutable: source, DestinationExecutable: source}); err != nil {
		t.Fatal(err)
	}
	hooksFile := filepath.Join(home, ".deepagents", "hooks.json")
	value, err := readJSONObject(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	hooks := value["hooks"].(map[string]any)
	stopGroups := hooks["Stop"].([]any)
	stopGroups = append(stopGroups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo keep"}}})
	hooks["Stop"] = stopGroups
	if err := writeJSONAtomic(hooksFile, value); err != nil {
		t.Fatal(err)
	}
	result, err := RemoveAdapter("dcode", home, RemoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.HookRemoved {
		t.Fatalf("managed dcode Hooks were not removed: %#v", result)
	}
	body, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "echo keep") || strings.Contains(string(body), "hook dcode") {
		t.Fatalf("unexpected remaining dcode Hooks: %s", body)
	}
}
