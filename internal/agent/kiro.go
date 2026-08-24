package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func kiroPlugin() Definition {
	return Definition{
		Name:                     "kiro",
		Backend:                  BackendBuiltin,
		BuiltinHookFile:          "~/.kiro/hooks/obs-agent-connector.json",
		PluginName:               "obs-agent-connector",
		AgentCommand:             "kiro-cli",
		DiscoveryCommandOptional: true,
		Markers: []string{
			"~/.kiro/hooks/obs-agent-connector.json",
		},
		ConfigFiles:      []string{"~/.obs-agent-connector/kiro/gtrace.json", "~/.kiro/gtrace.json"},
		EnabledJSONPath:  []string{"enabled"},
		ResolveInstall:   resolveKiroForInstall,
		ResolveDiscovery: resolveKiroForDiscovery,
	}
}

func resolveKiroForInstall(p Definition) (Definition, error) {
	if command, ok := resolveKiroCommandPath(); ok {
		p.AgentCommand = command
		return p, nil
	}
	if kiroDataExists() {
		return p, nil
	}
	return Definition{}, fmt.Errorf("kiro CLI was not found; start a Kiro CLI session before installing its adapter")
}

func resolveKiroForDiscovery(p Definition) (Definition, bool) {
	if command, ok := resolveKiroCommandPath(); ok {
		p.AgentCommand = command
		return p, true
	}
	if kiroDataExists() {
		return p, true
	}
	return Definition{}, false
}

func kiroDataExists() bool {
	return PathExists(ExpandHome("~/.kiro/session-index")) ||
		PathExists(ExpandHome("~/.kiro/sessions/cli"))
}

func resolveKiroCommandPath() (string, bool) {
	candidates := []string{
		strings.TrimSpace(os.Getenv("KIRO_CLI_BINARY")),
		strings.TrimSpace(os.Getenv("KIRO_CLI_PATH")),
	}
	if pathCommand, err := exec.LookPath("kiro-cli"); err == nil {
		candidates = append(candidates, pathCommand)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if absolute, err := filepath.Abs(candidate); err == nil {
			candidate = absolute
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}
