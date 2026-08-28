package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MinimumGrokVersion = "1.0.5"

var grokVersionPattern = regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z])v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)(?:$|[^0-9A-Za-z.+-])`)

type parsedGrokVersion struct {
	major      int
	minor      int
	patch      int
	prerelease string
}

func grokPlugin() Definition {
	return Definition{
		Name:                     "grok",
		Backend:                  BackendBuiltin,
		BuiltinHookFile:          "~/.grok/hooks/obs-agent-connector.json",
		PluginName:               "obs-agent-connector",
		AgentCommand:             "grok",
		SupportedPlatforms:       []string{"darwin", "linux", "windows"},
		DiscoveryCommandOptional: true,
		Markers: []string{
			"~/.grok/hooks/obs-agent-connector.json",
		},
		ConfigFiles:      []string{"~/.obs-agent-connector/grok/gtrace.json"},
		EnabledJSONPath:  []string{"enabled"},
		ResolveInstall:   resolveGrokForInstall,
		ResolveDiscovery: resolveGrokForDiscovery,
	}
}

func resolveGrokForInstall(p Definition) (Definition, error) {
	if command, ok := resolveGrokCommandPath(); ok {
		p.AgentCommand = command
		if _, err := ValidateGrokVersion(command); err != nil {
			return Definition{}, err
		}
		return p, nil
	}
	if PathExists(ExpandHome("~/.grok")) {
		return p, nil
	}
	return Definition{}, fmt.Errorf("grok was not found; install or start Grok Build before installing its adapter")
}

func resolveGrokForDiscovery(p Definition) (Definition, bool) {
	if command, ok := resolveGrokCommandPath(); ok {
		p.AgentCommand = command
		return p, true
	}
	if PathExists(ExpandHome("~/.grok")) {
		return p, true
	}
	return Definition{}, false
}

func resolveGrokCommandPath() (string, bool) {
	candidates := []string{
		strings.TrimSpace(os.Getenv("GROK_BINARY")),
		strings.TrimSpace(os.Getenv("GROK_CLI_PATH")),
	}
	if pathCommand, err := exec.LookPath("grok"); err == nil {
		candidates = append(candidates, pathCommand)
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
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

func detectGrokVersion(command string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, command, "--version").CombinedOutput()
	if err != nil {
		return "", false
	}
	return parseGrokVersion(string(output))
}

// DetectGrokVersion returns the normalized Grok Build version when the CLI
// exposes a parseable semantic version.
func DetectGrokVersion(command string) (string, bool) {
	if strings.TrimSpace(command) == "" || strings.TrimSpace(command) == "grok" {
		if resolved, ok := resolveGrokCommandPath(); ok {
			command = resolved
		}
	}
	return detectGrokVersion(command)
}

// ValidateGrokVersion rejects known unsupported versions while allowing an
// unavailable or unparseable version to proceed with a caller-visible warning.
func ValidateGrokVersion(command string) (bool, error) {
	version, known := DetectGrokVersion(command)
	if !known {
		return false, nil
	}
	if !grokVersionAtLeast(version, MinimumGrokVersion) {
		return true, fmt.Errorf("grok %s is not supported; upgrade Grok Build to %s or later", version, MinimumGrokVersion)
	}
	return true, nil
}

func parseGrokVersion(output string) (string, bool) {
	match := grokVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if len(match) != 2 {
		return "", false
	}
	if _, ok := parseGrokSemanticVersion(match[1]); !ok {
		return "", false
	}
	return match[1], true
}

func grokVersionAtLeast(version, minimum string) bool {
	current, currentOK := parseGrokSemanticVersion(version)
	required, requiredOK := parseGrokSemanticVersion(minimum)
	if !currentOK || !requiredOK {
		return false
	}
	for _, pair := range [][2]int{{current.major, required.major}, {current.minor, required.minor}, {current.patch, required.patch}} {
		if pair[0] != pair[1] {
			return pair[0] > pair[1]
		}
	}
	return current.prerelease == "" || required.prerelease != ""
}

func parseGrokSemanticVersion(value string) (parsedGrokVersion, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	withoutBuild, _, _ := strings.Cut(value, "+")
	core, prerelease, _ := strings.Cut(withoutBuild, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return parsedGrokVersion{}, false
	}
	numbers := make([]int, len(parts))
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return parsedGrokVersion{}, false
		}
		numbers[index] = number
	}
	return parsedGrokVersion{major: numbers[0], minor: numbers[1], patch: numbers[2], prerelease: prerelease}, true
}
