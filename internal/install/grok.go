package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/agentfiles"
)

var grokHookEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"PostToolUseFailure",
	"PermissionDenied",
	"Stop",
	"StopFailure",
	"StopCancelled",
	"Notification",
	"SubagentStart",
	"SubagentStop",
	"SessionEnd",
}

type GrokOptions struct {
	Home                  string
	SourceExecutable      string
	DestinationExecutable string
	HooksFile             string
	ConfigFile            string
	Endpoint              string
	TracePath             string
	MetricsPath           string
	InstallType           string
	XToken                string
	Headers               []string
	ResourceAttributes    []string
	CaptureContent        string
	MaxChars              int
	Enabled               *bool
	NoConfig              bool
}

type GrokResult struct {
	Executable string
	HooksFile  string
	ConfigFile string
	Configured bool
}

func InstallGrok(options GrokOptions) (GrokResult, error) {
	home := options.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return GrokResult{}, err
		}
	}
	source := options.SourceExecutable
	if source == "" {
		var err error
		source, err = os.Executable()
		if err != nil {
			return GrokResult{}, err
		}
	}
	destination := options.DestinationExecutable
	if destination == "" {
		name := "obs-agent-connector"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		destination = filepath.Join(home, ".local", "bin", name)
	}
	hooksFile := firstInstallPath(options.HooksFile, filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json"))
	configFile := firstInstallPath(options.ConfigFile, agentfiles.ConfigPath(home, "grok"))
	hooks, err := readJSONObject(hooksFile)
	if err != nil {
		return GrokResult{}, fmt.Errorf("parse Grok Hooks: %w", err)
	}
	configValue, configExists, err := readJSONObjectIfExists(configFile)
	if err != nil {
		return GrokResult{}, fmt.Errorf("parse Grok GTrace config: %w", err)
	}

	absoluteSource, err := filepath.Abs(source)
	if err != nil {
		return GrokResult{}, err
	}
	absoluteDestination, err := filepath.Abs(destination)
	if err != nil {
		return GrokResult{}, err
	}
	if absoluteSource != absoluteDestination {
		if err := copyExecutable(absoluteSource, absoluteDestination); err != nil {
			return GrokResult{}, err
		}
	}
	if err := writeGrokHooks(hooksFile, hooks, absoluteDestination); err != nil {
		return GrokResult{}, err
	}

	result := GrokResult{Executable: absoluteDestination, HooksFile: hooksFile, ConfigFile: configFile}
	if options.NoConfig {
		return result, nil
	}
	configOptions := CodexOptions{
		Endpoint: options.Endpoint, TracePath: options.TracePath, MetricsPath: options.MetricsPath,
		InstallType: options.InstallType, XToken: options.XToken, Headers: options.Headers,
		ResourceAttributes: options.ResourceAttributes, CaptureContent: options.CaptureContent,
		MaxChars: options.MaxChars, Enabled: options.Enabled,
	}
	if !shouldConfigureGTrace(configExists, configOptions) {
		return result, nil
	}
	next, err := mergeCodexGTraceConfig(configValue, configOptions, configExists)
	if err != nil {
		return result, err
	}
	if err := writeJSONAtomic(configFile, next); err != nil {
		return result, err
	}
	result.Configured = true
	return result, nil
}

func writeGrokHooks(path string, settings map[string]any, executable string) error {
	if settings == nil {
		settings = map[string]any{}
	}
	hooks, err := grokHooksObject(settings)
	if err != nil {
		return err
	}
	for _, event := range grokHookEvents {
		groups, err := grokHookGroups(hooks, event)
		if err != nil {
			return err
		}
		next, _ := removeManagedGrokHandlers(groups)
		group := map[string]any{
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": grokHookCommand(executable, event),
				"timeout": 5,
			}},
		}
		if event == "Notification" {
			group["matcher"] = "idle_prompt"
		}
		hooks[event] = append(next, group)
	}
	return writeJSONAtomic(path, settings)
}

type grokHookShell string

const (
	grokHookShellPOSIX      grokHookShell = "posix"
	grokHookShellPowerShell grokHookShell = "powershell"
	grokHookShellCMD        grokHookShell = "cmd"
	grokHookShellGitBash    grokHookShell = "bash"
)

func grokHookCommand(executable, event string) string {
	bashAvailable := false
	if runtime.GOOS == "windows" {
		bashAvailable = findGrokGitBash() != ""
	}
	return grokHookCommandForPlatform(executable, event, runtime.GOOS, os.Getenv("GROK_SHELL"), bashAvailable)
}

func grokHookCommandForPlatform(executable, event, goos, requestedShell string, bashAvailable bool) string {
	suffix := " hook grok " + event
	switch resolveGrokHookShell(goos, requestedShell, bashAvailable) {
	case grokHookShellPowerShell:
		// Grok routes Windows Hook commands through its selected shell. Both
		// PowerShell 7 and Windows PowerShell 5.1 require the call operator
		// when the executable path is quoted.
		return "& " + quotePowerShellLiteral(executable) + suffix
	case grokHookShellCMD:
		return quoteHookCommand(executable) + suffix
	case grokHookShellGitBash:
		return quotePOSIXShell(strings.ReplaceAll(executable, `\`, "/")) + suffix
	default:
		return quoteHookCommand(executable) + suffix
	}
}

func resolveGrokHookShell(goos, requestedShell string, bashAvailable bool) grokHookShell {
	if goos != "windows" {
		return grokHookShellPOSIX
	}
	switch strings.ToLower(strings.TrimSpace(requestedShell)) {
	case "cmd", "cmd.exe":
		return grokHookShellCMD
	case "bash", "gitbash", "git-bash":
		if bashAvailable {
			return grokHookShellGitBash
		}
		return grokHookShellPowerShell
	case "pwsh", "powershell":
		return grokHookShellPowerShell
	default:
		// This mirrors Grok Build's normal Windows cascade: pwsh first,
		// Windows PowerShell second, and PowerShell again as the final
		// fallback when no explicit recognized override is active.
		return grokHookShellPowerShell
	}
}

func quotePowerShellLiteral(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
}

func quotePOSIXShell(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `'"'"'`) + `'`
}

func findGrokGitBash() string {
	bases := []struct {
		root  string
		parts []string
	}{
		{root: os.Getenv("PROGRAMFILES"), parts: []string{"Git", "bin", "bash.exe"}},
		{root: os.Getenv("PROGRAMFILES(X86)"), parts: []string{"Git", "bin", "bash.exe"}},
		{root: os.Getenv("LOCALAPPDATA"), parts: []string{"Programs", "Git", "bin", "bash.exe"}},
	}
	for _, base := range bases {
		if base.root == "" {
			continue
		}
		candidate := filepath.Join(append([]string{base.root}, base.parts...)...)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if path, err := exec.LookPath("bash.exe"); err == nil && strings.Contains(strings.ToLower(path), "git") {
		return path
	}
	return ""
}

func grokHooksObject(settings map[string]any) (map[string]any, error) {
	value, exists := settings["hooks"]
	if !exists || value == nil {
		hooks := map[string]any{}
		settings["hooks"] = hooks
		return hooks, nil
	}
	hooks, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid Grok Hooks: hooks must be an object")
	}
	return hooks, nil
}

func grokHookGroups(hooks map[string]any, event string) ([]any, error) {
	value, exists := hooks[event]
	if !exists || value == nil {
		return nil, nil
	}
	groups, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Grok Hooks: %s must be an array", event)
	}
	return groups, nil
}

func managedGrokHandler(value any) bool {
	handler, ok := value.(map[string]any)
	if !ok {
		return false
	}
	command := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fmt.Sprint(handler["command"])), `\`, "/"))
	return strings.Contains(command, "hook grok") &&
		(strings.Contains(command, "obs-agent-connector") || strings.Contains(command, "grok-otel-plugin"))
}

func removeManagedGrokHandlers(groups []any) ([]any, bool) {
	next := make([]any, 0, len(groups))
	changed := false
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			next = append(next, rawGroup)
			continue
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			next = append(next, rawGroup)
			continue
		}
		kept := make([]any, 0, len(handlers))
		for _, handler := range handlers {
			if managedGrokHandler(handler) {
				changed = true
				continue
			}
			kept = append(kept, handler)
		}
		if len(kept) == 0 && len(handlers) > 0 {
			continue
		}
		if len(kept) != len(handlers) {
			copyGroup := make(map[string]any, len(group))
			for key, value := range group {
				copyGroup[key] = value
			}
			copyGroup["hooks"] = kept
			next = append(next, copyGroup)
			continue
		}
		next = append(next, rawGroup)
	}
	return next, changed
}
