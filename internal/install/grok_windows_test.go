//go:build windows

package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokWindowsHookCommandExecutesPowerShell7(t *testing.T) {
	program, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Skip("PowerShell 7 is not installed")
	}
	testGrokPowerShellHookCommand(t, program, "pwsh")
}

func TestGrokWindowsHookCommandExecutesWindowsPowerShell(t *testing.T) {
	program := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	if _, err := os.Stat(program); err != nil {
		t.Skip("Windows PowerShell 5.1 is not installed")
	}
	testGrokPowerShellHookCommand(t, program, "powershell")
}

func TestGrokWindowsHookCommandExecutesDefaultShell(t *testing.T) {
	program, err := exec.LookPath("pwsh.exe")
	if err != nil {
		program = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
		if _, statErr := os.Stat(program); statErr != nil {
			t.Skip("neither PowerShell 7 nor Windows PowerShell 5.1 is installed")
		}
	}
	testGrokPowerShellHookCommand(t, program, "")
}

func TestGrokWindowsHookCommandExecutesCMD(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Program Files", "Grok Hook Test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "hook-helper.cmd")
	marker := filepath.Join(t.TempDir(), "cmd-args.txt")
	script := "@echo off\r\n> \"%HOOK_MARKER%\" echo %*\r\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command := writeGrokWindowsHookCommand(t, helper, "cmd")
	cmd := exec.Command("cmd.exe", "/D", "/S", "/C", command)
	cmd.Env = append(os.Environ(), "HOOK_MARKER="+marker)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cmd Hook command failed: %v\n%s", err, output)
	}
	assertGrokHookArguments(t, marker)
}

func TestGrokWindowsHookCommandExecutesGitBash(t *testing.T) {
	program := findGrokGitBash()
	if program == "" {
		t.Skip("Git Bash is not installed")
	}
	root := filepath.Join(t.TempDir(), "Program Files", "Grok Hook Test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "hook-helper.sh")
	marker := filepath.Join(t.TempDir(), "bash-args.txt")
	script := "#!/usr/bin/env bash\nprintf '%s' \"$*\" > \"$HOOK_MARKER\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command := writeGrokWindowsHookCommand(t, helper, "bash")
	cmd := exec.Command(program, "-c", command)
	cmd.Env = append(os.Environ(),
		"HOOK_MARKER="+strings.ReplaceAll(marker, `\`, "/"),
		"MSYS_NO_PATHCONV=1",
		"MSYS2_ARG_CONV_EXCL=*",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Git Bash Hook command failed: %v\n%s", err, output)
	}
	assertGrokHookArguments(t, marker)
}

func testGrokPowerShellHookCommand(t *testing.T, program, requestedShell string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Program Files", "Grok Hook Test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "hook-helper.ps1")
	markerName := requestedShell
	if markerName == "" {
		markerName = "default"
	}
	marker := filepath.Join(t.TempDir(), markerName+"-args.txt")
	script := `param([Parameter(ValueFromRemainingArguments=$true)][string[]]$HookArgs)
[System.IO.File]::WriteAllText($env:HOOK_MARKER, ($HookArgs -join ' '))
`
	if err := os.WriteFile(helper, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	command := writeGrokWindowsHookCommand(t, helper, requestedShell)
	cmd := exec.Command(program, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	cmd.Env = append(os.Environ(), "HOOK_MARKER="+marker)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s Hook command failed: %v\n%s", requestedShell, err, output)
	}
	assertGrokHookArguments(t, marker)
}

func writeGrokWindowsHookCommand(t *testing.T, executable, requestedShell string) string {
	t.Helper()
	t.Setenv("GROK_SHELL", requestedShell)
	hooksFile := filepath.Join(t.TempDir(), "obs-agent-connector.json")
	if err := writeGrokHooks(hooksFile, map[string]any{}, executable); err != nil {
		t.Fatal(err)
	}
	value, err := readJSONObject(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	groups := value["hooks"].(map[string]any)["Stop"].([]any)
	handlers := groups[0].(map[string]any)["hooks"].([]any)
	return handlers[0].(map[string]any)["command"].(string)
}

func assertGrokHookArguments(t *testing.T, marker string) {
	t.Helper()
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(body)); got != "hook grok Stop" {
		t.Fatalf("Hook arguments = %q, want %q", got, "hook grok Stop")
	}
}
