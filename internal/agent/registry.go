package agent

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

var definitions = map[string]Definition{
	"claude":    claudePlugin(),
	"codebuddy": codeBuddyPlugin(),
	"codex":     codexPlugin(),
	"cursor":    cursorPlugin(),
	"dcode":     dcodePlugin(),
	"dsh":       dshPlugin(),
	"grok":      grokPlugin(),
	"hermes":    hermesPlugin(),
	"kiro":      kiroPlugin(),
	"opencode":  opencodePlugin(),
	"openclaw":  openClawPlugin(),
	"qoder":     qoderPlugin(),
	"qoder-cn":  qoderCNPlugin(),
	"workbuddy": workBuddyPlugin(),
}

func Get(name string) Definition {
	return definitions[name]
}

func Select(target string) ([]Definition, error) {
	target = strings.TrimSpace(strings.ToLower(target))
	if target == "" {
		names := Names()
		out := make([]Definition, 0, len(names))
		for _, name := range names {
			out = append(out, definitions[name])
		}
		return out, nil
	}

	p, ok := definitions[target]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q; available agents: %s", target, strings.Join(Names(), ", "))
	}
	return []Definition{p}, nil
}

func SelectInstalled(target string) ([]Definition, error) {
	normalizedTarget := strings.TrimSpace(strings.ToLower(target))
	selected, err := Select(target)
	if err != nil {
		return nil, err
	}

	out := make([]Definition, 0, len(selected))
	for _, p := range selected {
		p = Resolve(p)
		if _, ok := InstalledMarker(p); ok {
			out = append(out, p)
			continue
		}
		if normalizedTarget != "" {
			return nil, fmt.Errorf("%s plugin is not installed", p.Name)
		}
	}
	return out, nil
}

func ResolveForInstall(selected []Definition) ([]Definition, error) {
	resolved := make([]Definition, 0, len(selected))
	for _, p := range selected {
		if p.ResolveInstall != nil {
			var err error
			p, err = p.ResolveInstall(p)
			if err != nil {
				return nil, err
			}
		} else {
			p = Resolve(p)
		}
		resolved = append(resolved, p)
	}
	return resolved, nil
}

func ResolveForRemove(selected []Definition) []Definition {
	resolved := make([]Definition, 0, len(selected))
	for _, p := range selected {
		if p.ResolveRemove != nil {
			p = p.ResolveRemove(p)
		} else {
			p = Resolve(p)
		}
		resolved = append(resolved, p)
	}
	return resolved
}

func DiscoverCandidatesForOS(goos string) []Candidate {
	names := Names()
	out := make([]Candidate, 0, len(names))
	for _, name := range names {
		p := definitions[name]
		if !SupportsPlatform(p, goos) {
			continue
		}
		if p.ResolveDiscovery != nil {
			var ok bool
			p, ok = p.ResolveDiscovery(p)
			if !ok {
				continue
			}
		} else {
			p = Resolve(p)
		}
		command, ok := detectAgentCommand(p)
		if !ok {
			if !p.DiscoveryCommandOptional {
				continue
			}
			command = "data-dir"
		}
		installedPath, _ := InstalledMarker(p)
		candidate := Candidate{
			Plugin:           p,
			DetectedCmd:      command,
			InstalledPath:    installedPath,
			InstalledVersion: InstalledVersion(p),
			Supported:        true,
		}
		out = append(out, candidate)
	}
	return out
}

func DiscoverCandidates() []Candidate {
	return DiscoverCandidatesForOS("")
}

func Names() []string {
	names := make([]string, 0, len(definitions))
	for name, p := range definitions {
		if p.Hidden {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func SupportedNames(goos string) []string {
	names := make([]string, 0, len(definitions))
	for _, name := range Names() {
		if SupportsPlatform(definitions[name], goos) {
			names = append(names, name)
		}
	}
	return names
}

func Resolve(p Definition) Definition {
	if p.Resolve != nil {
		return p.Resolve(p)
	}
	return p
}

func SupportsPlatform(p Definition, goos string) bool {
	goos = strings.ToLower(strings.TrimSpace(goos))
	if goos == "" {
		return true
	}
	if p.IsBuiltin() {
		return goos == "linux" || goos == "darwin" || goos == "windows"
	}
	if len(p.SupportedPlatforms) > 0 {
		for _, platform := range p.SupportedPlatforms {
			if strings.EqualFold(strings.TrimSpace(platform), goos) {
				if goos != "windows" {
					return true
				}
				return strings.TrimSpace(p.WindowsInstaller) != ""
			}
		}
		return false
	}
	if goos == "windows" {
		return strings.TrimSpace(p.WindowsInstaller) != ""
	}
	return true
}

func detectAgentCommand(p Definition) (string, bool) {
	if _, err := exec.LookPath(p.AgentCommand); err == nil {
		return p.AgentCommand, true
	}
	return "", false
}
