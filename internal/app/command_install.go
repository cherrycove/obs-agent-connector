package app

import (
	"flag"
	"fmt"
	agent "github.com/GuanceCloud/obs-agent-connector/internal/agent"
	"os"
	"strings"
)

type repeatedValue []string

func (v *repeatedValue) String() string { return strings.Join(*v, ",") }
func (v *repeatedValue) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "", "GTrace endpoint")
	xToken := fs.String("x-token", "", "GTrace X-Token")
	agentID := fs.String("agent-id", "", "GTrace agent_id tag")
	agentName := fs.String("agent-name", "", "GTrace agent_name tag")
	tracePath := fs.String("trace-path", "", "Trace upload path for a built-in adapter")
	metricsPath := fs.String("metrics-path", "", "Metrics upload path for a built-in adapter")
	captureContent := fs.String("capture-content", "", "Built-in adapter content capture mode: none, preview, or full")
	maxChars := fs.Int("max-chars", 0, "Maximum captured characters per built-in adapter value")
	enable := fs.Bool("enable", false, "Enable telemetry in the Agent runtime config")
	disable := fs.Bool("disable", false, "Disable telemetry in the Agent runtime config")
	var headers repeatedValue
	var tags repeatedValue
	fs.Var(&headers, "header", "Built-in adapter HTTP header KEY=VALUE; may be repeated")
	fs.Var(&tags, "tag", "Resource attribute KEY=VALUE; may be repeated")
	staticBaseFlag := fs.String("static-base", "", "Installer script and plugin package base URL. OSS paths use the agent_plugins directory")
	yes := fs.Bool("yes", false, "Skip confirmation")
	dryRun := fs.Bool("dry-run", false, "Print commands without installing")

	target := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		flagArgs = args[1:]
	}

	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unrecognized install arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *enable && *disable {
		return fmt.Errorf("--enable and --disable cannot be used together")
	}
	if *maxChars < 0 {
		return fmt.Errorf("--max-chars must be positive")
	}
	mode := strings.ToLower(strings.TrimSpace(*captureContent))
	if mode != "" && mode != "none" && mode != "preview" && mode != "full" {
		return fmt.Errorf("unsupported --capture-content %q", *captureContent)
	}
	if err := validateAssignments(headers, "--header"); err != nil {
		return err
	}
	if err := validateAssignments(tags, "--tag"); err != nil {
		return err
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("install requires a single agent, for example: install codex")
	}

	selected, err := agent.Select(target)
	if err != nil {
		return err
	}
	if !agent.SupportsPlatform(selected[0], currentGOOS) {
		return unsupportedPlatformError(selected[0], currentGOOS)
	}
	selected, err = agent.ResolveForInstall(selected)
	if err != nil {
		return err
	}

	cfg, _, err := loadConnectorConfig()
	if err != nil {
		return err
	}

	inputDefaults := installInput{
		Endpoint:       strings.TrimSpace(*endpoint),
		TracePath:      strings.Trim(strings.TrimSpace(*tracePath), "/"),
		MetricsPath:    strings.Trim(strings.TrimSpace(*metricsPath), "/"),
		XToken:         strings.TrimSpace(*xToken),
		Headers:        append([]string{}, headers...),
		AgentID:        strings.TrimSpace(*agentID),
		AgentName:      strings.TrimSpace(*agentName),
		GlobalTags:     append([]string{}, tags...),
		CaptureContent: mode,
		MaxChars:       *maxChars,
	}
	if *enable || *disable {
		value := *enable
		inputDefaults.Enabled = &value
	}
	var input installInput
	if selected[0].IsBuiltin() {
		input, err = resolveInstallInput(inputDefaults, cfg, selected[0].Name)
	} else {
		input, err = resolveExternalInstallInput(inputDefaults, cfg, selected[0].Name)
	}
	if err != nil {
		return err
	}

	var pluginDownload pluginDownloadConfig
	if !selected[0].IsBuiltin() {
		pluginDownload, err = pluginDownloadSettings(*staticBaseFlag, cfg, input.Endpoint)
		if err != nil {
			return err
		}
	}
	fmt.Println()
	fmt.Println("Install plan:")
	targets := make([]string, 0, len(selected))
	for _, p := range selected {
		if p.IsBuiltin() {
			targets = append(targets, fmt.Sprintf("%s (built into obs-agent-connector)", p.Name))
			continue
		}
		url, err := downloadSourceURL(pluginDownload, p, currentGOOS)
		if err != nil {
			return err
		}
		targets = append(targets, fmt.Sprintf("%s (%s)", p.Name, url))
	}
	rows := [][2]string{
		{"Targets", strings.Join(targets, ", ")},
		{"Type", fixedType},
		{"Endpoint", input.Endpoint},
		{"X-Token", "<configured>"},
	}
	if !selected[0].IsBuiltin() {
		rows = append(rows, [2]string{"Plugin Source", pluginDownload.Source}, [2]string{"Plugin Base URL", pluginDownload.BaseURL})
	}
	if len(input.GlobalTags) > 0 {
		rows = append(rows, [2]string{"Global Tags", strings.Join(input.GlobalTags, ", ")})
	}
	rows = append(rows,
		[2]string{"Agent ID", input.AgentID},
		[2]string{"Agent Name", input.AgentName},
	)
	printDetails(rows)

	if *dryRun {
		fmt.Println()
		fmt.Println("Command preview:")
		for _, p := range selected {
			if p.IsBuiltin() {
				fmt.Printf("register %s hook with the current obs-agent-connector runtime\n", p.Name)
				continue
			}
			preview := input
			preview.XToken = "<redacted>"
			fmt.Println(renderInstallCommand(pluginDownload, p, preview))
		}
		return nil
	}

	if !*yes {
		ok, err := confirm("Continue installation?", true)
		if err != nil {
			return err
		}
		if !ok {
			printSingleDetail("Result", "canceled")
			return nil
		}
	}

	for _, p := range selected {
		if err := installOne(pluginDownload, p, input); err != nil {
			return err
		}
	}

	return nil
}

func validateAssignments(values []string, flagName string) error {
	for _, value := range values {
		key, item, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(item) == "" {
			return fmt.Errorf("%s must use non-empty KEY=VALUE syntax: %q", flagName, value)
		}
	}
	return nil
}
