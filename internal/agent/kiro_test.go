package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKiroInstallRequiresCommandOrDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("KIRO_CLI_BINARY", "")
	t.Setenv("KIRO_CLI_PATH", "")
	if _, err := resolveKiroForInstall(definitions["kiro"]); err == nil {
		t.Fatal("expected Kiro install resolution to reject a missing product")
	}
	if err := os.MkdirAll(filepath.Join(home, ".kiro", "sessions", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveKiroForInstall(definitions["kiro"]); err != nil {
		t.Fatalf("expected Kiro data directory fallback: %v", err)
	}
}

func TestKiroInstallRecognizesModernSessionIndex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("KIRO_CLI_BINARY", "")
	t.Setenv("KIRO_CLI_PATH", "")
	if err := os.MkdirAll(filepath.Join(home, ".kiro", "session-index"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveKiroForInstall(definitions["kiro"]); err != nil {
		t.Fatalf("expected modern Kiro data directory fallback: %v", err)
	}
}

func TestKiroDiscoveryUsesResolvedCommand(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	command := filepath.Join(binDir, "kiro-cli")
	if err := os.WriteFile(command, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	t.Setenv("KIRO_CLI_BINARY", "")
	t.Setenv("KIRO_CLI_PATH", "")
	resolved, ok := resolveKiroForDiscovery(definitions["kiro"])
	if !ok || resolved.AgentCommand != command {
		t.Fatalf("unexpected Kiro discovery result: ok=%t definition=%#v", ok, resolved)
	}
}
