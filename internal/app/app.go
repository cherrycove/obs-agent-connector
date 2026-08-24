package app

import "fmt"

const (
	appName            = "obs-agent-connector"
	fixedType          = "gtrace"
	defaultStaticBase  = "https://static.guance.com"
	pluginOSSDirectory = "agent_plugins"
	pluginSourceOSS    = "oss"
	pluginSourceGitHub = "github"
	configDirName      = ".obs-agent-connector"
	configFileName     = "config.json"
)

var version = "dev"

func Run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "list":
		return listPlugins(args[1:])
	case "status":
		return status(args[1:])
	case "discover":
		return discover(args[1:])
	case "install":
		return install(args[1:])
	case "config":
		return configCommand(args[1:])
	case "enable":
		return enable(args[1:])
	case "disable":
		return disable(args[1:])
	case "update":
		return update(args[1:])
	case "remove":
		return remove(args[1:])
	case "uninstall":
		return uninstallConnector(args[1:])
	case "version":
		return showVersion(args[1:])
	case "internal":
		if len(args) >= 2 && args[1] == "merge-config" {
			return mergeConnectorConfig(args[2:])
		}
		return fmt.Errorf("unknown internal command")
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Printf(`%s

Usage:
  obs-agent-connector <command> [arguments]

Commands:
  list                  List installed Agent plugins
  status <agent>        Show one Agent plugin status
  discover              Detect local Agents; install missing plugins, or sync all with -u
  install <agent>       Install an Agent plugin
  config <agent>        List or edit one Agent runtime config
  enable <agent>        Enable one installed Agent plugin
  disable <agent>       Disable one installed Agent plugin
  update <agent>        Update one installed Agent plugin
  remove <agent>        Remove an Agent plugin
  uninstall             Uninstall obs-agent-connector and its managed built-in Hooks
  version               Show version and check for updates

Examples:
  obs-agent-connector discover
  obs-agent-connector discover -u
  obs-agent-connector status codex
  obs-agent-connector install codex
  obs-agent-connector install codebuddy
  obs-agent-connector install dcode
  obs-agent-connector install dsh
  obs-agent-connector install cursor
  obs-agent-connector install kiro
  obs-agent-connector config codex list
  obs-agent-connector config codex edit --enabled=false
  obs-agent-connector install opencode
  obs-agent-connector install qoder
  obs-agent-connector enable codex
  obs-agent-connector disable codex
  obs-agent-connector update codex
  obs-agent-connector remove codex
  obs-agent-connector uninstall
  obs-agent-connector version

`, appName)
}
