package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func dcodePlugin() Definition {
	return Definition{
		Name:                     "dcode",
		Backend:                  BackendBuiltin,
		BuiltinHookFile:          "~/.deepagents/hooks.json",
		PluginName:               "obs-agent-connector",
		AgentCommand:             "dcode",
		DiscoveryCommandOptional: true,
		Markers: []string{
			"~/.deepagents/hooks.json",
		},
		ConfigFiles:     []string{"~/.obs-agent-connector/dcode/gtrace.json", "~/.deepagents/gtrace.json"},
		EnabledJSONPath: []string{"enabled"},
		RemoveCleanupDetails: []string{
			"~/.deepagents/hooks.json (remove managed dcode Hook handlers)",
		},
		ResolveInstall:   resolveDcodeForInstall,
		ResolveDiscovery: resolveDcodeForDiscovery,
	}
}

func resolveDcodeForInstall(p Definition) (Definition, error) {
	if command, ok := resolveDcodeCommandPath(); ok {
		p.AgentCommand = command
		return p, nil
	}
	if PathExists(ExpandHome("~/.deepagents")) {
		return p, nil
	}
	return Definition{}, fmt.Errorf("dcode was not found; install or start Deep Agents Code before installing its adapter")
}

func resolveDcodeForDiscovery(p Definition) (Definition, bool) {
	if command, ok := resolveDcodeCommandPath(); ok {
		p.AgentCommand = command
		return p, true
	}
	if PathExists(ExpandHome("~/.deepagents")) {
		return p, true
	}
	return Definition{}, false
}

func resolveDcodeCommandPath() (string, bool) {
	candidates := []string{
		strings.TrimSpace(os.Getenv("DCODE_BINARY")),
		strings.TrimSpace(os.Getenv("DEEPAGENTS_CODE_BINARY")),
		strings.TrimSpace(os.Getenv("DCODE_CLI_PATH")),
	}
	for _, name := range []string{"dcode", "deepagents-code"} {
		if pathCommand, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, pathCommand)
		}
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if absolute, err := filepath.Abs(candidate); err == nil {
			candidate = absolute
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}
