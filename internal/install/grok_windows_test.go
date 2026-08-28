//go:build windows

package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_GROK_HOOK_HELPER") == "1" {
		marker := os.Getenv("HOOK_MARKER")
		if marker == "" {
			os.Exit(2)
		}
		if err := os.WriteFile(marker, []byte(strings.Join(os.Args[1:], " ")), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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
	helper := grokWindowsHookHelperExecutable(t)
	marker := filepath.Join(t.TempDir(), "cmd-args.txt")
	command := writeGrokWindowsHookCommand(t, helper, "cmd")
	cmd := exec.Command("cmd.exe", "/C", command)
	cmd.Env = grokWindowsHookHelperEnvironment(marker)
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
	helper := grokWindowsHookHelperExecutable(t)
	marker := filepath.Join(t.TempDir(), "bash-args.txt")
	command := writeGrokWindowsHookCommand(t, helper, "bash")
	cmd := exec.Command(program, "-c", command)
	cmd.Env = append(grokWindowsHookHelperEnvironment(strings.ReplaceAll(marker, `\`, "/")),
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
	helper := grokWindowsHookHelperExecutable(t)
	markerName := requestedShell
	if markerName == "" {
		markerName = "default"
	}
	marker := filepath.Join(t.TempDir(), markerName+"-args.txt")
	command := writeGrokWindowsHookCommand(t, helper, requestedShell)
	cmd := exec.Command(program, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	cmd.Env = grokWindowsHookHelperEnvironment(marker)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s Hook command failed: %v\n%s", requestedShell, err, output)
	}
	assertGrokHookArguments(t, marker)
}

func grokWindowsHookHelperExecutable(t *testing.T) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "Program Files", "Grok Hook Test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "hook-helper.exe")
	if err := copyExecutable(source, destination); err != nil {
		t.Fatal(err)
	}
	return destination
}

func grokWindowsHookHelperEnvironment(marker string) []string {
	return append(os.Environ(),
		"GO_WANT_GROK_HOOK_HELPER=1",
		"HOOK_MARKER="+marker,
	)
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
