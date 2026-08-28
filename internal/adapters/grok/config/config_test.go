package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUsesManagedConfigAndGrokEnvironment(t *testing.T) {
	home := t.TempDir()
	managedDir := filepath.Join(home, ".obs-agent-connector", "grok")
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "gtrace.json"), []byte(`{"enabled":false,"endpoint":"https://config.example","captureContent":"none","resourceAttributes":{"team":"platform"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Resolve(ResolveOptions{Home: home, Env: map[string]string{
		"GROK_OTEL_ENABLED":  "true",
		"GROK_OTEL_ENDPOINT": "https://env.example",
	}})
	if !cfg.Enabled || cfg.Transport.Endpoint != "https://env.example" {
		t.Fatalf("unexpected resolved config: %#v", cfg)
	}
	if cfg.CaptureContent != "none" || cfg.ResourceAttributes["team"] != "platform" {
		t.Fatalf("managed fields were not preserved: %#v", cfg)
	}
	if cfg.ResourceAttributes["service.name"] != "gtrace-grok" || cfg.ResourceAttributes["agent_runtime"] != "grok" {
		t.Fatalf("missing Grok resource defaults: %#v", cfg.ResourceAttributes)
	}
	if cfg.LogFile != filepath.Join(managedDir, "gtrace-hooks.json") || cfg.StateDir != filepath.Join(managedDir, "state") {
		t.Fatalf("unexpected managed paths: %#v", cfg)
	}
}

func TestResolveDisabledWithoutEndpoint(t *testing.T) {
	cfg := Resolve(ResolveOptions{Home: t.TempDir(), Env: map[string]string{}})
	if cfg.Enabled {
		t.Fatalf("empty Grok configuration must be disabled: %#v", cfg)
	}
}
