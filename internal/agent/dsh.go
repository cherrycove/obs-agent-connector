package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func dshPlugin() Definition {
	return Definition{
		Name:                     "dsh",
		PluginName:               "dsh-otel-plugin",
		AgentCommand:             "dsh",
		WindowsInstaller:         "install-release.ps1",
		InstallArgs:              []string{"--profile", "web"},
		WindowsArgs:              []string{"-Profile", "web"},
		DiscoveryCommandOptional: true,
		Markers: []string{
			"~/.dsh/profiles/web/node_modules/dsh-otel-plugin",
			"~/.dsh/profiles/node_modules/dsh-otel-plugin",
		},
		ConfigFiles:     []string{"~/.dsh/gtrace.json"},
		EnabledJSONPath: []string{"enabled"},
		RemoveCmds: [][]string{
			{"dsh", "plugin", "--profile", "web", "remove", "dsh-otel-plugin"},
		},
		RemoveFallbackCmd: []string{"npx", "--yes", "@deepseek-ai/dsh"},
		RemovePaths: []string{
			"~/.dsh/profiles/web/node_modules/dsh-otel-plugin",
			"~/.dsh/profiles/node_modules/dsh-otel-plugin",
		},
		Resolve:          resolveDshPlugin,
		ResolveInstall:   resolveDshForInstall,
		ResolveDiscovery: resolveDshForDiscovery,
	}
}

func resolveDshPlugin(p Definition) Definition { return withDshProfile(p, dshProfile()) }

func resolveDshForInstall(p Definition) (Definition, error) {
	resolved := resolveDshPlugin(p)
	if command, ok := resolveDshCommandPath(); ok {
		resolved.AgentCommand = command
		return resolved, nil
	}
	return Definition{}, fmt.Errorf("dsh CLI was not found; install DeepSeek Harness CLI or set DSH_BINARY before installing its plugin")
}

func resolveDshForDiscovery(p Definition) (Definition, bool) {
	resolved := resolveDshPlugin(p)
	if command, ok := resolveDshCommandPath(); ok {
		resolved.AgentCommand = command
		return resolved, true
	}
	if PathExists(ExpandHome(dshHome())) {
		return resolved, true
	}
	return Definition{}, false
}

func resolveDshCommandPath() (string, bool) {
	candidates := []string{
		strings.TrimSpace(os.Getenv("DSH_BINARY")),
		strings.TrimSpace(os.Getenv("DEEPSEEK_HARNESS_BINARY")),
		strings.TrimSpace(os.Getenv("DSH_CLI_PATH")),
	}
	if pathCommand, err := exec.LookPath("dsh"); err == nil {
		candidates = append(candidates, pathCommand)
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

func withDshProfile(p Definition, profile string) Definition {
	resolved := p
	home := dshHome()
	if profile = strings.TrimSpace(profile); profile == "" {
		profile = "web"
	}
	profileRoot := filepath.ToSlash(filepath.Join(home, "profiles", profile))
	resolved.Env = []string{"DSH_HOME=" + home}
	resolved.PackageArgs = []string{"--profile", profile}
	resolved.InstallArgs = []string{"--profile", profile}
	resolved.WindowsArgs = []string{"-Profile", profile}
	resolved.RemoveCmds = [][]string{{"dsh", "plugin", "--profile", profile, "remove", "dsh-otel-plugin"}}
	resolved.Markers = []string{profileRoot + "/node_modules/dsh-otel-plugin", filepath.ToSlash(filepath.Join(home, "profiles", "node_modules", "dsh-otel-plugin"))}
	resolved.RemovePaths = append([]string{}, resolved.Markers...)
	resolved.ConfigFiles = []string{home + "/gtrace.json"}
	resolved.EnabledJSONPath = []string{"enabled"}
	return resolved
}

func dshHome() string {
	if value := strings.TrimSpace(os.Getenv("DSH_HOME")); value != "" {
		return value
	}
	return "~/.dsh"
}

func dshProfile() string {
	if value := strings.TrimSpace(os.Getenv("DSH_PROFILE")); value != "" {
		return value
	}
	return "web"
}
