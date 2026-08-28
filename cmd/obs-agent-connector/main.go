package main

import (
	"fmt"
	"os"

	claudehook "github.com/GuanceCloud/obs-agent-connector/internal/adapters/claude/hook"
	codebuddyhook "github.com/GuanceCloud/obs-agent-connector/internal/adapters/codebuddy/hook"
	codexhook "github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/hook"
	cursorhook "github.com/GuanceCloud/obs-agent-connector/internal/adapters/cursor/hook"
	dcodehook "github.com/GuanceCloud/obs-agent-connector/internal/adapters/dcode/hook"
	grokhook "github.com/GuanceCloud/obs-agent-connector/internal/adapters/grok/hook"
	kirohook "github.com/GuanceCloud/obs-agent-connector/internal/adapters/kiro/hook"
	"github.com/GuanceCloud/obs-agent-connector/internal/app"
)

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "hook" {
		switch os.Args[2] {
		case "claude":
			os.Exit(claudehook.RunCLI())
		case "codebuddy":
			os.Exit(codebuddyhook.RunCLI(os.Args[3:]))
		case "codex":
			os.Exit(codexhook.RunCLI())
		case "cursor":
			os.Exit(cursorhook.RunCLI(os.Args[3:]))
		case "dcode":
			os.Exit(dcodehook.RunCLI(os.Args[3:]))
		case "grok":
			os.Exit(grokhook.RunCLI(os.Args[3:]))
		case "kiro":
			os.Exit(kirohook.RunCLI(os.Args[3:]))
		}
	}
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
