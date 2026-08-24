package app

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	agent "github.com/GuanceCloud/obs-agent-connector/internal/agent"
	telemetryinstall "github.com/GuanceCloud/obs-agent-connector/internal/install"
)

var installCodexAdapter = telemetryinstall.InstallCodex

func installBuiltinAdapter(p agent.Definition, input installInput, noConfig bool) error {
	executable, err := currentExecutable()
	if err != nil {
		return fmt.Errorf("resolve connector executable: %w", err)
	}
	printSingleDetail("Runtime", executable)
	switch p.Name {
	case "claude":
		_, err = telemetryinstall.InstallClaude(telemetryinstall.ClaudeOptions{
			SourceExecutable:      executable,
			DestinationExecutable: executable,
			Endpoint:              input.Endpoint,
			TracePath:             input.TracePath,
			MetricsPath:           input.MetricsPath,
			InstallType:           fixedType,
			XToken:                input.XToken,
			Headers:               append([]string{}, input.Headers...),
			ResourceAttributes:    builtinResourceAttributes(input),
			CaptureContent:        input.CaptureContent,
			MaxChars:              input.MaxChars,
			Enabled:               input.Enabled,
			NoConfig:              noConfig,
		})
	case "codebuddy":
		_, err = telemetryinstall.InstallCodeBuddy(telemetryinstall.CodeBuddyOptions{
			SourceExecutable: executable, DestinationExecutable: executable,
			Endpoint: input.Endpoint, TracePath: input.TracePath, MetricsPath: input.MetricsPath,
			InstallType: fixedType, XToken: input.XToken, Headers: append([]string{}, input.Headers...),
			ResourceAttributes: builtinResourceAttributes(input), CaptureContent: input.CaptureContent,
			MaxChars: input.MaxChars, Enabled: input.Enabled, NoConfig: noConfig,
		})
		if err == nil {
			printSingleDetail("Note", "Restart CodeBuddy if the reconciled Hook is not picked up automatically.")
		}
	case "codex":
		result, installErr := installCodexAdapter(telemetryinstall.CodexOptions{
			SourceExecutable:      executable,
			DestinationExecutable: executable,
			CodexCommand:          p.AgentCommand,
			Endpoint:              input.Endpoint,
			TracePath:             input.TracePath,
			MetricsPath:           input.MetricsPath,
			InstallType:           fixedType,
			XToken:                input.XToken,
			Headers:               append([]string{}, input.Headers...),
			ResourceAttributes:    builtinResourceAttributes(input),
			CaptureContent:        input.CaptureContent,
			MaxChars:              input.MaxChars,
			Enabled:               input.Enabled,
			NoConfig:              noConfig,
		})
		err = installErr
		if err == nil {
			if result.TrustSkipped {
				printSingleDetail("Trust", "skipped")
			} else {
				printSingleDetail("Trust", "granted")
			}
		}
	case "cursor":
		_, err = telemetryinstall.InstallCursor(telemetryinstall.CursorOptions{
			SourceExecutable:      executable,
			DestinationExecutable: executable,
			Endpoint:              input.Endpoint,
			TracePath:             input.TracePath,
			MetricsPath:           input.MetricsPath,
			InstallType:           fixedType,
			XToken:                input.XToken,
			Headers:               append([]string{}, input.Headers...),
			ResourceAttributes:    builtinResourceAttributes(input),
			CaptureContent:        input.CaptureContent,
			MaxChars:              input.MaxChars,
			Enabled:               input.Enabled,
			NoConfig:              noConfig,
		})
	case "dcode":
		_, err = telemetryinstall.InstallDcode(telemetryinstall.DcodeOptions{
			SourceExecutable:      executable,
			DestinationExecutable: executable,
			Endpoint:              input.Endpoint,
			TracePath:             input.TracePath,
			MetricsPath:           input.MetricsPath,
			InstallType:           fixedType,
			XToken:                input.XToken,
			Headers:               append([]string{}, input.Headers...),
			ResourceAttributes:    builtinResourceAttributes(input),
			CaptureContent:        input.CaptureContent,
			MaxChars:              input.MaxChars,
			Enabled:               input.Enabled,
			NoConfig:              noConfig,
		})
		if err == nil {
			printSingleDetail("Note", "Start a new dcode session or run /reload to load the reconciled Hooks.")
		}
	case "kiro":
		_, err = telemetryinstall.InstallKiro(telemetryinstall.KiroOptions{
			SourceExecutable:      executable,
			DestinationExecutable: executable,
			Endpoint:              input.Endpoint,
			TracePath:             input.TracePath,
			MetricsPath:           input.MetricsPath,
			InstallType:           fixedType,
			XToken:                input.XToken,
			Headers:               append([]string{}, input.Headers...),
			ResourceAttributes:    builtinResourceAttributes(input),
			CaptureContent:        input.CaptureContent,
			MaxChars:              input.MaxChars,
			Enabled:               input.Enabled,
			NoConfig:              noConfig,
		})
	default:
		return fmt.Errorf("%s does not have a built-in telemetry adapter", p.Name)
	}
	if err != nil {
		return fmt.Errorf("install built-in %s adapter: %w", p.Name, err)
	}
	return nil
}

func removeBuiltinAdapter(p agent.Definition, options telemetryinstall.RemoveOptions) error {
	result, err := telemetryinstall.RemoveAdapter(p.Name, "", options)
	if err != nil {
		return err
	}
	printSingleDetail("Hook", removedOrKept(result.HookRemoved))
	printSingleDetail("Config", removedOrKept(result.ConfigRemoved))
	if options.PurgeManaged {
		printSingleDetail("Managed Files", removedOrKept(result.ManagedFilesRemoved))
	}
	if options.PurgeState {
		printSingleDetail("State", removedOrKept(result.StatePurged))
	}
	if p.Name == "claude" || p.Name == "codex" || p.Name == "cursor" {
		removeBuiltinLegacyResidue(p)
	}
	return nil
}

func removeBuiltinLegacyResidue(p agent.Definition) {
	for _, command := range p.RemoveCmds {
		if len(command) == 0 {
			continue
		}
		if _, err := exec.LookPath(command[0]); err != nil {
			printSingleDetail("Skip", fmt.Sprintf("%s was not found: %s", command[0], strings.Join(command, " ")))
			continue
		}
		printSingleDetail("Command", strings.Join(command, " "))
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			printSingleDetail("Warning", fmt.Sprintf("legacy %s removal command failed; continuing: %v", p.Name, err))
		}
	}

	if p.RemoveCleanup != nil {
		for _, item := range p.RemoveCleanupDetails {
			printSingleDetail("Cleanup", item)
		}
		if err := p.RemoveCleanup(p); err != nil {
			printSingleDetail("Warning", fmt.Sprintf("legacy %s cleanup failed; continuing: %v", p.Name, err))
		}
	}

	for _, path := range p.RemovePaths {
		expanded := agent.ExpandHome(path)
		if !agent.PathExists(expanded) {
			continue
		}
		printSingleDetail("Cleanup", agent.DisplayPath(expanded))
		if err := os.RemoveAll(expanded); err != nil {
			printSingleDetail("Warning", fmt.Sprintf("failed to remove %s; continuing: %v", agent.DisplayPath(expanded), err))
		}
	}
}

func removedOrKept(removed bool) string {
	if removed {
		return "removed"
	}
	return "kept"
}

func builtinResourceAttributes(input installInput) []string {
	values := append([]string{}, input.GlobalTags...)
	if strings.TrimSpace(input.AgentID) != "" {
		values = append(values, "agent_id="+input.AgentID)
	}
	if strings.TrimSpace(input.AgentName) != "" {
		values = append(values, "agent_name="+input.AgentName)
	}
	return values
}

func installedPluginVersion(p agent.Definition) string {
	if p.IsBuiltin() {
		return version
	}
	return agent.InstalledVersion(p)
}
