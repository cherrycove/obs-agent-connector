package app

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	agent "github.com/GuanceCloud/obs-agent-connector/internal/agent"
	telemetryinstall "github.com/GuanceCloud/obs-agent-connector/internal/install"
)

var currentExecutable = os.Executable
var currentEvalSymlinks = filepath.EvalSymlinks

func uninstallConnector(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	yes := fs.Bool("yes", false, "Skip confirmation")
	dryRun := fs.Bool("dry-run", false, "Print what would be removed")
	keepConfig := fs.Bool("keep-config", false, "Keep connector config files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unrecognized uninstall arguments: %s", strings.Join(fs.Args(), " "))
	}

	executablePath, err := currentExecutable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	if resolved, err := currentEvalSymlinks(executablePath); err == nil {
		executablePath = resolved
	}

	configPath, err := connectorConfigPath()
	if err != nil {
		return err
	}

	installDir := filepath.Dir(executablePath)
	pathLine := fmt.Sprintf("export PATH=\"%s:$PATH\"", installDir)
	unixPathFiles := pathCleanupCandidates(pathLine)

	fmt.Println("Uninstall plan:")
	rows := make([][2]string, 0, 4+len(unixPathFiles))
	rows = append(rows, [2]string{"Binary", executablePath})
	managedFilesMode := "remove managed config, logs, and state"
	if *keepConfig {
		managedFilesMode = "keep managed config; remove Hooks, logs, and state"
	}
	rows = append(rows, [2]string{"Built-in Agents", "remove claude, codebuddy, codex, cursor, dcode, and kiro; " + managedFilesMode})
	if *keepConfig {
		rows = append(rows, [2]string{"Config", "keep " + configPath})
	} else {
		rows = append(rows, [2]string{"Config", "remove " + configPath})
	}
	if currentGOOS == "windows" {
		rows = append(rows, [2]string{"User PATH", "remove " + installDir + " if present"})
	} else {
		if len(unixPathFiles) == 0 {
			rows = append(rows, [2]string{"Shell PATH", "no managed entry found"})
		} else {
			for _, path := range unixPathFiles {
				rows = append(rows, [2]string{"Shell PATH", "remove managed entry from " + path})
			}
		}
	}
	printDetails(rows)

	if *dryRun {
		return nil
	}

	if !*yes {
		ok, err := confirm("Continue uninstall?", false)
		if err != nil {
			return err
		}
		if !ok {
			printSingleDetail("Result", "canceled")
			return nil
		}
	}
	for _, adapter := range []string{"claude", "codebuddy", "codex", "cursor", "dcode", "kiro"} {
		selected := agent.ResolveForRemove([]agent.Definition{agent.Get(adapter)})
		if len(selected) == 0 {
			continue
		}
		if err := removeBuiltinAdapter(selected[0], telemetryinstall.RemoveOptions{
			PurgeState:   true,
			PurgeManaged: !*keepConfig,
		}); err != nil {
			printSingleDetail("Warning", fmt.Sprintf("failed to remove built-in %s; continuing uninstall: %v", adapter, err))
		}
	}

	if currentGOOS == "windows" {
		if err := removeInstallDirFromWindowsUserPath(installDir); err != nil {
			return err
		}
	} else {
		for _, path := range unixPathFiles {
			if err := removePathLineFromFile(path, pathLine); err != nil {
				return err
			}
		}
	}

	if !*keepConfig {
		if err := removeFileIfExists(configPath); err != nil {
			return err
		}
		if err := removeDirIfEmpty(filepath.Dir(configPath)); err != nil {
			return err
		}
	}

	if currentGOOS == "windows" {
		if err := scheduleWindowsSelfDelete(executablePath); err != nil {
			return err
		}
		printSingleDetail("Result", "uninstall scheduled")
		printSingleDetail("Note", "Close this shell after the command exits if the executable remains locked.")
		return nil
	}

	if err := removeFileIfExists(executablePath); err != nil {
		return err
	}
	printSingleDetail("Result", "uninstalled")
	return nil
}

func pathCleanupCandidates(pathLine string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	candidates := []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".profile"),
	}
	out := make([]string, 0, len(candidates))
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), pathLine) {
			out = append(out, path)
		}
	}
	return out
}

func removePathLineFromFile(path string, pathLine string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	updated, changed := removePathLineFromContent(string(data), pathLine)
	if !changed {
		return nil
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

func removePathLineFromContent(content string, pathLine string) (string, bool) {
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		if strings.TrimSpace(line) == pathLine {
			changed = true
			continue
		}
		filtered = append(filtered, line)
	}
	if !changed {
		return content, false
	}
	return strings.Join(filtered, "\n"), true
}

func removeInstallDirFromWindowsUserPath(installDir string) error {
	current := os.Getenv("PATH")
	updated, changed := removePathEntry(current, installDir, ";")
	if changed {
		_ = os.Setenv("PATH", updated)
	}

	userPath, err := windowsReadUserPath()
	if err != nil {
		return err
	}
	nextUserPath, changed := removePathEntry(userPath, installDir, ";")
	if !changed {
		return nil
	}
	return windowsWriteUserPath(nextUserPath)
}

func removePathEntry(pathValue string, installDir string, separator string) (string, bool) {
	parts := strings.Split(pathValue, separator)
	filtered := make([]string, 0, len(parts))
	changed := false
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if strings.EqualFold(trimmed, installDir) {
			changed = true
			continue
		}
		filtered = append(filtered, trimmed)
	}
	return strings.Join(filtered, separator), changed
}

func windowsReadUserPath() (string, error) {
	cmd := exec.Command("reg", "query", `HKCU\Environment`, "/v", "Path")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read user PATH: %w", err)
	}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "path") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		return strings.Join(fields[2:], " "), nil
	}
	return "", nil
}

func windowsWriteUserPath(value string) error {
	cmd := exec.Command("reg", "add", `HKCU\Environment`, "/v", "Path", "/t", "REG_EXPAND_SZ", "/d", value, "/f")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("write user PATH: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func scheduleWindowsSelfDelete(executablePath string) error {
	command := fmt.Sprintf(`ping 127.0.0.1 -n 2 > nul & del /f /q "%s"`, windowsCommandEscape(executablePath))
	cmd := exec.Command("cmd.exe", "/c", command)
	return cmd.Start()
}

func windowsCommandEscape(value string) string {
	return strings.ReplaceAll(value, `"`, `\"`)
}

func removeFileIfExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Remove(path)
}

func removeDirIfEmpty(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) > 0 {
		return nil
	}
	return os.Remove(path)
}
