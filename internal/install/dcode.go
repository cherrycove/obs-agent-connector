package install

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/agentfiles"
)

var dcodeHookEvents = []string{
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"PostToolUseFailure",
	"Stop",
	"SessionEnd",
	"SubagentStart",
	"SubagentStop",
}

type DcodeOptions struct {
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

type DcodeResult struct {
	Executable string
	HooksFile  string
	ConfigFile string
	Configured bool
}

func InstallDcode(options DcodeOptions) (DcodeResult, error) {
	home := options.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return DcodeResult{}, err
		}
	}
	source := options.SourceExecutable
	if source == "" {
		var err error
		source, err = os.Executable()
		if err != nil {
			return DcodeResult{}, err
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
	hooksFile := firstInstallPath(options.HooksFile, filepath.Join(home, ".deepagents", "hooks.json"))
	configFile := firstInstallPath(options.ConfigFile, agentfiles.ConfigPath(home, "dcode"))
	hooks, err := readJSONObject(hooksFile)
	if err != nil {
		return DcodeResult{}, fmt.Errorf("parse dcode Hooks: %w", err)
	}
	configValue, configExists, err := readJSONObjectIfExists(configFile)
	if err != nil {
		return DcodeResult{}, fmt.Errorf("parse dcode GTrace config: %w", err)
	}
	if !configExists && options.ConfigFile == "" {
		configValue, configExists, err = readJSONObjectIfExists(filepath.Join(home, ".deepagents", "gtrace.json"))
		if err != nil {
			return DcodeResult{}, fmt.Errorf("parse legacy dcode GTrace config: %w", err)
		}
	}

	absoluteSource, err := filepath.Abs(source)
	if err != nil {
		return DcodeResult{}, err
	}
	absoluteDestination, err := filepath.Abs(destination)
	if err != nil {
		return DcodeResult{}, err
	}
	if absoluteSource != absoluteDestination {
		if err := copyExecutable(absoluteSource, absoluteDestination); err != nil {
			return DcodeResult{}, err
		}
	}
	if err := writeDcodeHooks(hooksFile, hooks, absoluteDestination); err != nil {
		return DcodeResult{}, err
	}

	result := DcodeResult{Executable: absoluteDestination, HooksFile: hooksFile, ConfigFile: configFile}
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

func writeDcodeHooks(path string, settings map[string]any, executable string) error {
	if settings == nil {
		settings = map[string]any{}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	for _, event := range dcodeHookEvents {
		groups, _ := hooks[event].([]any)
		next, _ := removeManagedDcodeHandlers(groups)
		next = append(next, map[string]any{
			"hooks": []any{map[string]any{
				"type": "command", "command": quoteHookCommand(executable) + " hook dcode " + event,
				"timeout": 5,
			}},
		})
		hooks[event] = next
	}
	return writeJSONWatched(path, settings)
}

func managedDcodeHandler(value any) bool {
	handler, ok := value.(map[string]any)
	if !ok {
		return false
	}
	command := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fmt.Sprint(handler["command"])), `\`, "/"))
	return strings.Contains(command, "dcode-otel-plugin") ||
		(strings.Contains(command, "hook dcode") &&
			(strings.Contains(command, "obs-agent-connector") || strings.Contains(command, "agent-telemetry")))
}

func removeManagedDcodeHandlers(groups []any) ([]any, bool) {
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
			if managedDcodeHandler(handler) {
				changed = true
				continue
			}
			kept = append(kept, handler)
		}
		if len(kept) == 0 {
			if len(handlers) > 0 {
				continue
			}
			next = append(next, rawGroup)
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
