package app

import (
	agent "github.com/GuanceCloud/obs-agent-connector/internal/agent"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var uuidPattern = regexp.MustCompile(`^agid_[0-9a-f]{32}$`)

func TestDefaultAgentNameIncludesAgentAndDate(t *testing.T) {
	name := defaultAgentName("claude", time.Date(2026, time.July, 15, 10, 30, 0, 0, time.Local))
	if !strings.HasSuffix(name, "_claude_20260715") {
		t.Fatalf("expected agent and date suffix, got %q", name)
	}
}

func TestGenerateAgentIDUsesUUIDv4HexWithoutDashes(t *testing.T) {
	agentID, err := generateAgentID()
	if err != nil {
		t.Fatal(err)
	}
	if !uuidPattern.MatchString(agentID) {
		t.Fatalf("expected agid_<uuidhex> agent_id, got %q", agentID)
	}
}

func TestConfirmUsesDefaultYesOnEOF(t *testing.T) {
	previousInput := confirmInput
	previousOutput := confirmOutput
	t.Cleanup(func() {
		confirmInput = previousInput
		confirmOutput = previousOutput
	})

	var output strings.Builder
	confirmInput = strings.NewReader("")
	confirmOutput = &output

	ok, err := confirm("Continue installation?", true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected EOF to accept the default yes confirmation")
	}
	if got := output.String(); got != "Continue installation? [Y/n]: " {
		t.Fatalf("unexpected prompt output %q", got)
	}
}

func TestConfirmUsesDefaultNoOnEOF(t *testing.T) {
	previousInput := confirmInput
	previousOutput := confirmOutput
	t.Cleanup(func() {
		confirmInput = previousInput
		confirmOutput = previousOutput
	})

	var output strings.Builder
	confirmInput = strings.NewReader("")
	confirmOutput = &output

	ok, err := confirm("Continue removal?", false)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected EOF to keep the default no confirmation")
	}
	if got := output.String(); got != "Continue removal? [y/N]: " {
		t.Fatalf("unexpected prompt output %q", got)
	}
}

func TestResolveInstallInputKeepsExplicitAgentName(t *testing.T) {
	input, err := resolveInstallInput(installInput{AgentName: "custom_name"}, connectorConfig{
		Endpoint: "https://llm-openway.guance.com",
		XToken:   "agent_test",
	}, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if input.AgentName != "custom_name" {
		t.Fatalf("expected explicit agent name to be kept, got %q", input.AgentName)
	}
}

func TestResolveInstallInputLoadsGlobalTagsFromConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input, err := resolveInstallInput(installInput{}, connectorConfig{
		Endpoint:   "https://llm-openway.guance.com",
		XToken:     "agent_test",
		GlobalTags: []string{"team=platform", "env=prod"},
	}, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.GlobalTags) != 2 || input.GlobalTags[0] != "team=platform" || input.GlobalTags[1] != "env=prod" {
		t.Fatalf("expected global tags from config, got %#v", input.GlobalTags)
	}
}

func TestResolveInstallInputPreservesExistingAgentIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".codex", "gtrace.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{
  "endpoint":"https://existing.example.com",
  "tracePath":"existing/traces",
  "metricsPath":"existing/metrics",
  "headers":{"X-Token":"existing-token","Authorization":"keep"},
  "captureContent":"none",
  "max_chars":321,
  "enabled":false,
  "resourceAttributes":{"agent_id":"agid_1234567890abcdef1234567890abcdef","agent_name":"existing-name","env":"existing"}
}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := resolveInstallInput(installInput{}, connectorConfig{
		Endpoint: "https://llm-openway.guance.com",
		XToken:   "agent_test",
	}, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if input.AgentID != "agid_1234567890abcdef1234567890abcdef" || input.AgentName != "existing-name" {
		t.Fatalf("existing identity was not preserved: %#v", input)
	}
	if input.Endpoint != "https://existing.example.com" || input.XToken != "existing-token" || input.TracePath != "existing/traces" || input.MetricsPath != "existing/metrics" {
		t.Fatalf("existing transport config was not preserved: %#v", input)
	}
	if input.CaptureContent != "none" || input.MaxChars != 321 || input.Enabled == nil || *input.Enabled {
		t.Fatalf("existing privacy config was not preserved: %#v", input)
	}
	if strings.Join(input.GlobalTags, ",") != "env=existing" || strings.Join(input.Headers, ",") != "Authorization=keep,X-Token=existing-token" {
		t.Fatalf("existing headers/resource attributes were not preserved: %#v", input)
	}
}

func TestResolveInstallInputRegeneratesInvalidExistingAgentID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".dsh", "gtrace.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"resourceAttributes":{"agent_id":"fffffffffffff","agent_name":"dsh-test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := resolveInstallInput(installInput{}, connectorConfig{
		Endpoint: "https://llm-openway.guance.com",
		XToken:   "agent_test",
	}, "dsh")
	if err != nil {
		t.Fatal(err)
	}
	if !uuidPattern.MatchString(input.AgentID) {
		t.Fatalf("expected invalid existing ID to be replaced, got %q", input.AgentID)
	}
}

func TestResolveInstallInputRejectsInvalidExplicitAgentID(t *testing.T) {
	_, err := resolveInstallInput(installInput{AgentID: "fffffffffffff"}, connectorConfig{
		Endpoint: "https://llm-openway.guance.com",
		XToken:   "agent_test",
	}, "dsh")
	if err == nil || !strings.Contains(err.Error(), "invalid agent_id") {
		t.Fatalf("expected invalid explicit agent ID error, got %v", err)
	}
}

func TestResolveExternalInstallInputDoesNotReadAgentRuntimeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".dsh", "gtrace.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"endpoint":"https://stale.example.com","resourceAttributes":{"agent_id":"fffffffffffff","agent_name":"stale"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := resolveExternalInstallInput(installInput{}, connectorConfig{
		Endpoint: "https://configured.example.com",
		XToken:   "agent_test",
	}, "dsh")
	if err != nil {
		t.Fatal(err)
	}
	if input.Endpoint != "https://configured.example.com" || input.AgentName == "stale" || !uuidPattern.MatchString(input.AgentID) {
		t.Fatalf("external install input read Agent-private config: %#v", input)
	}
}

func TestResolveInstallInputExplicitTransportOverridesExistingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".codex", "gtrace.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"endpoint":"https://existing.example.com","headers":{"X-Token":"existing-token","Authorization":"keep"}}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := resolveInstallInput(installInput{
		Endpoint: "https://explicit.example.com",
		XToken:   "explicit-token",
	}, connectorConfig{}, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if input.Endpoint != "https://explicit.example.com" || input.XToken != "explicit-token" {
		t.Fatalf("explicit transport values did not win: %#v", input)
	}
	if strings.Contains(strings.Join(input.Headers, ","), "existing-token") {
		t.Fatalf("legacy X-Token remained in merged headers: %#v", input.Headers)
	}
}

func TestInstallerURLForWindowsUsesOSSReleaseScript(t *testing.T) {
	definition := agentDefinitionForTest("openclaw")
	url, err := installerURLForOS(pluginDownloadConfig{Source: pluginSourceOSS, BaseURL: "https://static.example.com"}, definition, "windows")
	if err != nil {
		t.Fatal(err)
	}
	expected := "https://static.example.com/agent_plugins/openclaw-otel-plugin/install-release.ps1"
	if url != expected {
		t.Fatalf("expected %q, got %q", expected, url)
	}
}

func TestInstallerURLForWindowsUsesGitHubReleaseScript(t *testing.T) {
	definition := agentDefinitionForTest("openclaw")
	url, err := installerURLForOS(pluginDownloadConfig{Source: pluginSourceGitHub, BaseURL: "https://github.com/GuanceCloud"}, definition, "windows")
	if err != nil {
		t.Fatal(err)
	}
	expected := "https://github.com/GuanceCloud/openclaw-otel-plugin/releases/latest/download/install-release.ps1"
	if url != expected {
		t.Fatalf("expected %q, got %q", expected, url)
	}
}

func TestRenderInstallCommandForWindowsUsesPowerShell(t *testing.T) {
	previous := currentGOOS
	currentGOOS = "windows"
	t.Cleanup(func() {
		currentGOOS = previous
	})

	command := renderInstallCommand(pluginDownloadConfig{Source: pluginSourceOSS, BaseURL: "https://static.example.com"}, agentDefinitionForTest("openclaw"), installInput{
		Endpoint:   "https://llm-openway.guance.com",
		XToken:     "agent_test",
		AgentID:    "agent_123",
		AgentName:  "demo_openclaw_20260721",
		GlobalTags: []string{"team=platform"},
	})

	if !strings.Contains(command, "install-release.ps1") {
		t.Fatalf("expected PowerShell release installer in command %q", command)
	}
	if !strings.Contains(command, "-Type 'gtrace'") {
		t.Fatalf("expected Windows openclaw command to include -Type gtrace, got %q", command)
	}
	if strings.Contains(command, "OSS_ENDPOINT=") {
		t.Fatalf("expected Windows command to avoid OSS shell env, got %q", command)
	}
	if !strings.Contains(command, "team=platform") {
		t.Fatalf("expected Windows command to include global tag, got %q", command)
	}
}

func TestRenderInstallCommandUsesNormalizedOSSBase(t *testing.T) {
	previous := currentGOOS
	currentGOOS = "linux"
	t.Cleanup(func() {
		currentGOOS = previous
	})

	command := renderInstallCommand(pluginDownloadConfig{Source: pluginSourceOSS, BaseURL: "https://static.example.com"}, agentDefinitionForTest("openclaw"), installInput{
		Endpoint:  "https://llm-openway.guance.com",
		XToken:    "agent_test",
		AgentID:   "agid_123",
		AgentName: "demo",
	})
	for _, want := range []string{
		"https://static.example.com/agent_plugins/openclaw-otel-plugin/install.sh",
		"OSS_ENDPOINT=https://static.example.com/agent_plugins",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("expected command to contain %q, got %q", want, command)
		}
	}
}

func TestBuildInstallArgsIncludesGlobalTagsBeforeAgentIdentity(t *testing.T) {
	args := buildInstallArgs("/tmp/install.sh", agentDefinitionForTest("codex"), installInput{
		Endpoint:   "https://llm-openway.guance.com",
		XToken:     "agent_test",
		AgentID:    "agid_1234567890abcdef1234567890abcdef",
		AgentName:  "demo_codex_20260727",
		GlobalTags: []string{"team=platform", "env=prod"},
	})
	want := []string{
		"--tag", "team=platform",
		"--tag", "env=prod",
		"--tag", "agent_id=agid_1234567890abcdef1234567890abcdef",
		"--tag", "agent_name=demo_codex_20260727",
	}
	joined := strings.Join(args, " ")
	for _, item := range want {
		if !strings.Contains(joined, item) {
			t.Fatalf("expected install args to contain %q, got %#v", item, args)
		}
	}
}

func TestDshUsesStandardExternalInstallerContract(t *testing.T) {
	p := agentDefinitionForTest("dsh")
	args := buildInstallArgs("/tmp/install.sh", p, installInput{
		Endpoint: "https://example.com",
		XToken:   "token",
		GlobalTags: []string{
			"agent_id=agid_test",
		},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"latest", "--type gtrace", "--endpoint https://example.com", "--x-token token", "--tag agent_id=agid_test", "--profile web"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected standard installer argument %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "--source") {
		t.Fatalf("standard external installer must resolve its own archive: %q", joined)
	}
}

func TestDshUsesStandardInstallerURLs(t *testing.T) {
	p := agentDefinitionForTest("dsh")
	for _, tc := range []struct {
		name   string
		source string
		base   string
		want   string
	}{
		{name: "oss", source: pluginSourceOSS, base: "https://static.example.com", want: "https://static.example.com/agent_plugins/dsh-otel-plugin/install.sh"},
		{name: "github", source: pluginSourceGitHub, base: "https://github.com/GuanceCloud", want: "https://github.com/GuanceCloud/dsh-otel-plugin/releases/latest/download/install-release.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := installerURLForOS(pluginDownloadConfig{Source: tc.source, BaseURL: tc.base}, p, "linux")
			if err != nil || got != tc.want {
				t.Fatalf("installer URL = %q, err = %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestCacheBustedLatestURLOnlyTouchesGitHubLatestAssets(t *testing.T) {
	latest := cacheBustedLatestURL("https://github.com/GuanceCloud/dsh-otel-plugin/releases/latest/download/install-release.sh")
	if !strings.HasPrefix(latest, "https://github.com/GuanceCloud/dsh-otel-plugin/releases/latest/download/install-release.sh?cachebust=") {
		t.Fatalf("unexpected cache-busted URL %q", latest)
	}
	static := "https://static.example.com/agent_plugins/dsh-otel-plugin/install.sh"
	if got := cacheBustedLatestURL(static); got != static {
		t.Fatalf("static installer URL changed: %q", got)
	}
}

func TestUnsupportedPlatformErrorForWindows(t *testing.T) {
	err := unsupportedPlatformError(agentDefinitionForTest("hermes"), "windows")
	if err == nil {
		t.Fatal("expected unsupported Windows error")
	}
	message := err.Error()
	if !strings.Contains(message, "hermes is not supported on Windows") {
		t.Fatalf("unexpected error message %q", message)
	}
	if !strings.Contains(message, "codex, cursor, dcode, dsh, kiro, openclaw, opencode, qoder, workbuddy") {
		t.Fatalf("expected supported Windows agent list in %q", message)
	}
}

func TestDownloadSourceURLUsesOSSArchiveForQoderOnUnix(t *testing.T) {
	url, err := downloadSourceURL(pluginDownloadConfig{Source: pluginSourceOSS, BaseURL: "https://static.example.com"}, agentDefinitionForTest("qoder"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	expected := "https://static.example.com/agent_plugins/qoder-otel-plugin/qoder-otel-plugin.tar.gz"
	if url != expected {
		t.Fatalf("expected %q, got %q", expected, url)
	}
}

func TestRenderPluginUpdateCommandUsesOSSArchiveForQoder(t *testing.T) {
	previous := currentGOOS
	currentGOOS = "linux"
	t.Cleanup(func() {
		currentGOOS = previous
	})

	command := renderPluginUpdateCommand(pluginDownloadConfig{Source: pluginSourceOSS, BaseURL: "https://static.example.com"}, agentDefinitionForTest("qoder"))
	if !strings.Contains(command, "https://static.example.com/agent_plugins/qoder-otel-plugin/qoder-otel-plugin.tar.gz") {
		t.Fatalf("expected qoder OSS archive in command %q", command)
	}
	if strings.Contains(command, "github.com") {
		t.Fatalf("expected qoder update command to avoid GitHub in %q", command)
	}
}

func TestDownloadSourceURLUsesGitHubArchiveForOpencodeOnUnix(t *testing.T) {
	url, err := downloadSourceURL(pluginDownloadConfig{Source: pluginSourceGitHub, BaseURL: "https://github.com/GuanceCloud"}, agentDefinitionForTest("opencode"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	expected := "https://github.com/GuanceCloud/opencode-otel-plugin/releases/latest/download/opencode-otel-plugin.tar.gz"
	if url != expected {
		t.Fatalf("expected %q, got %q", expected, url)
	}
}

func TestDownloadSourceURLUsesOSSArchiveForOpencodeOnUnix(t *testing.T) {
	url, err := downloadSourceURL(pluginDownloadConfig{Source: pluginSourceOSS, BaseURL: "https://static.example.com"}, agentDefinitionForTest("opencode"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	expected := "https://static.example.com/agent_plugins/opencode-otel-plugin/opencode-otel-plugin.tar.gz"
	if url != expected {
		t.Fatalf("expected %q, got %q", expected, url)
	}
}

func TestOSSDownloadURLsUseAgentPluginsDirectory(t *testing.T) {
	for _, tc := range []struct {
		agent string
		goos  string
		path  string
	}{
		{agent: "dsh", goos: "linux", path: "dsh-otel-plugin/install.sh"},
		{agent: "dsh", goos: "windows", path: "dsh-otel-plugin/install-release.ps1"},
		{agent: "hermes", goos: "linux", path: "hermes-otel-plugin/install.sh"},
		{agent: "opencode", goos: "linux", path: "opencode-otel-plugin/opencode-otel-plugin.tar.gz"},
		{agent: "opencode", goos: "windows", path: "opencode-otel-plugin/install-release.ps1"},
		{agent: "openclaw", goos: "linux", path: "openclaw-otel-plugin/install.sh"},
		{agent: "openclaw", goos: "windows", path: "openclaw-otel-plugin/install-release.ps1"},
		{agent: "qoder", goos: "linux", path: "qoder-otel-plugin/qoder-otel-plugin.tar.gz"},
		{agent: "qoder", goos: "windows", path: "qoder-otel-plugin/install-release.ps1"},
		{agent: "workbuddy", goos: "darwin", path: "workbuddy-otel-plugin/workbuddy-otel-plugin.tar.gz"},
		{agent: "workbuddy", goos: "windows", path: "workbuddy-otel-plugin/install-release.ps1"},
	} {
		t.Run(tc.agent+"_"+tc.goos, func(t *testing.T) {
			got, err := downloadSourceURL(
				pluginDownloadConfig{Source: pluginSourceOSS, BaseURL: "https://static.example.com"},
				agentDefinitionForTest(tc.agent),
				tc.goos,
			)
			if err != nil {
				t.Fatal(err)
			}
			want := "https://static.example.com/agent_plugins/" + tc.path
			if got != want {
				t.Fatalf("expected %q, got %q", want, got)
			}
		})
	}
}

func agentDefinitionForTest(name string) agent.Definition {
	definition := agent.Get(name)
	if definition.Name == "" {
		panic("missing test agent definition: " + name)
	}
	return definition
}
