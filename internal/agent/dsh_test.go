package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDshProfileResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	t.Setenv("DSH_PROFILE", "headless")
	resolved := Resolve(dshPlugin())
	want := filepath.ToSlash(filepath.Join(home, "profiles", "headless")) + "/node_modules/dsh-otel-plugin"
	if resolved.Markers[0] != want || resolved.ConfigFiles[0] != home+"/gtrace.json" {
		t.Fatalf("unexpected DSH paths: markers=%v config=%v", resolved.Markers, resolved.ConfigFiles)
	}
	if got := resolved.PackageArgs; len(got) != 2 || got[1] != "headless" {
		t.Fatalf("unexpected DSH package args: %v", got)
	}
}

func TestDshDiscoveryFromHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	if resolved, ok := resolveDshForDiscovery(dshPlugin()); !ok || resolved.Name != "dsh" {
		t.Fatalf("expected DSH discovery from home, got %#v, %t", resolved, ok)
	}
}

func TestDshInstallRequiresCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DSH_BINARY", "")
	t.Setenv("DEEPSEEK_HARNESS_BINARY", "")
	t.Setenv("DSH_CLI_PATH", "")

	if _, err := resolveDshForInstall(dshPlugin()); err == nil {
		t.Fatal("expected DSH install resolution to reject a missing CLI")
	}
}

func TestDshInstallUsesResolvedCommand(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	pathDir := t.TempDir()
	command := filepath.Join(binDir, "dsh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", pathDir)
	t.Setenv("DSH_BINARY", command)
	t.Setenv("DEEPSEEK_HARNESS_BINARY", "")
	t.Setenv("DSH_CLI_PATH", "")

	resolved, err := resolveDshForInstall(dshPlugin())
	if err != nil {
		t.Fatalf("expected DSH install resolution to succeed: %v", err)
	}
	if resolved.AgentCommand != command {
		t.Fatalf("expected resolved DSH command %q, got %q", command, resolved.AgentCommand)
	}
	wantPath := "PATH=" + binDir + string(os.PathListSeparator) + pathDir
	if got := resolved.Env[len(resolved.Env)-1]; got != wantPath {
		t.Fatalf("expected injected PATH %q, got %q", wantPath, got)
	}
}

func TestDshInstallUsesCachedNpxCommand(t *testing.T) {
	home := t.TempDir()
	cacheBin := filepath.Join(home, ".npm", "_npx", "cache-id", "node_modules", ".bin")
	command := filepath.Join(cacheBin, "dsh")
	if err := os.MkdirAll(cacheBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", pathDir)
	t.Setenv("DSH_BINARY", "")
	t.Setenv("DEEPSEEK_HARNESS_BINARY", "")
	t.Setenv("DSH_CLI_PATH", "")

	resolved, err := resolveDshForInstall(dshPlugin())
	if err != nil {
		t.Fatalf("expected cached DSH command to resolve: %v", err)
	}
	if resolved.AgentCommand != command {
		t.Fatalf("expected cached DSH command %q, got %q", command, resolved.AgentCommand)
	}
	wantPath := "PATH=" + cacheBin + string(os.PathListSeparator) + pathDir
	if got := resolved.Env[len(resolved.Env)-1]; got != wantPath {
		t.Fatalf("expected injected PATH %q, got %q", wantPath, got)
	}
}
