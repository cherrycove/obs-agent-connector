package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveReadsKiroConfigAndEnvironmentOverrides(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kiro", "gtrace.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"endpoint":"https://config.example.com","enabled":false,"captureContent":"none","resourceAttributes":{"team":"platform"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Resolve(ResolveOptions{Home: home, Cwd: t.TempDir(), Env: map[string]string{
		"KIRO_OTEL_ENABLED":  "true",
		"KIRO_OTEL_ENDPOINT": "https://env.example.com",
	}})
	if !cfg.Enabled || cfg.Transport.Endpoint != "https://env.example.com" {
		t.Fatalf("unexpected resolved config: %#v", cfg)
	}
	if cfg.CaptureContent != "none" || cfg.ResourceAttributes["team"] != "platform" {
		t.Fatalf("Kiro config fields were not preserved: %#v", cfg)
	}
}

func TestResolveUsesManagedPaths(t *testing.T) {
	home := t.TempDir()
	managedDir := filepath.Join(home, ".obs-agent-connector", "kiro")
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedDir, "gtrace.json"), []byte(`{"enabled":true,"endpoint":"https://managed.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Resolve(ResolveOptions{Home: home, Cwd: t.TempDir(), Env: map[string]string{}})
	if !cfg.Enabled || cfg.Transport.Endpoint != "https://managed.example" {
		t.Fatalf("managed config was not loaded: %#v", cfg)
	}
	if want := filepath.Join(managedDir, "gtrace-hooks.json"); cfg.LogFile != want {
		t.Fatalf("LogFile = %q, want %q", cfg.LogFile, want)
	}
	if want := filepath.Join(home, ".kiro", "sessions"); cfg.SessionDir != want {
		t.Fatalf("SessionDir = %q, want %q", cfg.SessionDir, want)
	}
}
