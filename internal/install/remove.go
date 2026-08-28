package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/agentfiles"
)

type RemoveResult struct {
	Adapter             string
	HookFile            string
	ConfigFile          string
	HookRemoved         bool
	TrustRemoved        bool
	ConfigRemoved       bool
	StatePurged         bool
	ManagedFilesRemoved bool
}

type RemoveOptions struct {
	PurgeConfig   bool
	PurgeState    bool
	PurgeManaged  bool
	ConnectorOnly bool
}

func RemoveAdapter(adapter, home string, options RemoveOptions) (RemoveResult, error) {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return RemoveResult{}, err
		}
	}
	var result RemoveResult
	var err error
	switch adapter {
	case "claude":
		result, err = removeClaude(home, options)
	case "codebuddy":
		result, err = removeCodeBuddy(home, options)
	case "codex":
		result, err = removeCodex(home, options)
	case "cursor":
		result, err = removeCursor(home, options)
	case "dcode":
		result, err = removeDcode(home, options)
	case "grok":
		result, err = removeGrok(home, options)
	case "kiro":
		result, err = removeKiro(home, options)
	default:
		return RemoveResult{}, errors.New("unsupported adapter " + adapter)
	}
	if err != nil {
		return result, err
	}
	if options.PurgeManaged {
		managedDir := agentfiles.Directory(home, adapter)
		if err := os.RemoveAll(managedDir); err != nil {
			return result, fmt.Errorf("remove managed %s files: %w", adapter, err)
		}
		result.ConfigRemoved = true
		result.ManagedFilesRemoved = true
	}
	return result, nil
}

func removeGrok(home string, options RemoveOptions) (RemoveResult, error) {
	result := RemoveResult{
		Adapter:    "grok",
		HookFile:   filepath.Join(home, ".grok", "hooks", "obs-agent-connector.json"),
		ConfigFile: agentfiles.ConfigPath(home, "grok"),
	}
	value, exists, err := readJSONObjectIfExists(result.HookFile)
	if err != nil {
		return result, err
	}
	if exists {
		hooks, err := grokHooksObject(value)
		if err != nil {
			return result, err
		}
		for _, event := range grokHookEvents {
			groups, err := grokHookGroups(hooks, event)
			if err != nil {
				return result, err
			}
			next, changed := removeManagedGrokHandlers(groups)
			if !changed {
				continue
			}
			result.HookRemoved = true
			if len(next) == 0 {
				delete(hooks, event)
			} else {
				hooks[event] = next
			}
		}
		if result.HookRemoved {
			if len(hooks) == 0 {
				delete(value, "hooks")
			}
			if len(value) == 0 {
				if err := removeFileIfExists(result.HookFile); err != nil {
					return result, err
				}
			} else if err := writeJSONAtomic(result.HookFile, value); err != nil {
				return result, err
			}
		}
	}
	if options.PurgeConfig {
		if err := removeConfigFiles(result.ConfigFile); err != nil {
			return result, err
		}
		result.ConfigRemoved = true
	}
	if options.PurgeState {
		if err := os.RemoveAll(filepath.Join(agentfiles.Directory(home, "grok"), "state")); err != nil {
			return result, err
		}
		if err := removeFileIfExists(agentfiles.HookLogPath(home, "grok")); err != nil {
			return result, err
		}
		result.StatePurged = true
	}
	return result, nil
}

func removeDcode(home string, options RemoveOptions) (RemoveResult, error) {
	result := RemoveResult{
		Adapter:    "dcode",
		HookFile:   filepath.Join(home, ".deepagents", "hooks.json"),
		ConfigFile: agentfiles.ConfigPath(home, "dcode"),
	}
	value, exists, err := readJSONObjectIfExists(result.HookFile)
	if err != nil {
		return result, err
	}
	if exists {
		hooks, _ := value["hooks"].(map[string]any)
		for _, event := range dcodeHookEvents {
			groups, _ := hooks[event].([]any)
			next, changed := removeManagedDcodeHandlers(groups)
			if changed {
				hooks[event] = next
				result.HookRemoved = true
			}
		}
		if result.HookRemoved {
			if err := writeJSONWatched(result.HookFile, value); err != nil {
				return result, err
			}
		}
	}
	if options.PurgeConfig {
		if err := removeConfigFiles(result.ConfigFile, filepath.Join(home, ".deepagents", "gtrace.json")); err != nil {
			return result, err
		}
		result.ConfigRemoved = true
	}
	if options.PurgeState {
		if err := os.RemoveAll(filepath.Join(agentfiles.Directory(home, "dcode"), "state")); err != nil {
			return result, err
		}
		if err := removeFileIfExists(agentfiles.HookLogPath(home, "dcode")); err != nil {
			return result, err
		}
		result.StatePurged = true
	}
	return result, nil
}

func removeKiro(home string, options RemoveOptions) (RemoveResult, error) {
	result := RemoveResult{
		Adapter:    "kiro",
		HookFile:   filepath.Join(home, ".kiro", "hooks", "obs-agent-connector.json"),
		ConfigFile: agentfiles.ConfigPath(home, "kiro"),
	}
	value, exists, err := readJSONObjectIfExists(result.HookFile)
	if err != nil {
		return result, err
	}
	if exists {
		entries, _ := value["hooks"].([]any)
		next := make([]any, 0, len(entries))
		for _, entry := range entries {
			if managedKiroHook(entry) {
				result.HookRemoved = true
				continue
			}
			next = append(next, entry)
		}
		if result.HookRemoved {
			if len(next) == 0 {
				if err := removeFileIfExists(result.HookFile); err != nil {
					return result, err
				}
			} else {
				value["hooks"] = next
				if err := writeJSONAtomic(result.HookFile, value); err != nil {
					return result, err
				}
			}
		}
	}
	if options.PurgeConfig {
		if err := removeConfigFiles(result.ConfigFile, filepath.Join(home, ".kiro", "gtrace.json")); err != nil {
			return result, err
		}
		result.ConfigRemoved = true
	}
	if options.PurgeState {
		if err := os.RemoveAll(filepath.Join(home, ".kiro", "gtrace")); err != nil {
			return result, err
		}
		if err := removeFileIfExists(agentfiles.HookLogPath(home, "kiro")); err != nil {
			return result, err
		}
		result.StatePurged = true
	}
	return result, nil
}

func removeCursor(home string, options RemoveOptions) (RemoveResult, error) {
	result := RemoveResult{Adapter: "cursor", HookFile: filepath.Join(home, ".cursor", "hooks.json"), ConfigFile: agentfiles.ConfigPath(home, "cursor")}
	settings, exists, err := readJSONObjectIfExists(result.HookFile)
	if err != nil {
		return result, err
	}
	if exists {
		hooks, _ := settings["hooks"].(map[string]any)
		managed := managedCursorHook
		if options.ConnectorOnly {
			managed = connectorManagedCursorHook
		}
		for _, event := range cursorHookEvents {
			entries, _ := hooks[event].([]any)
			next := make([]any, 0, len(entries))
			changed := false
			for _, entry := range entries {
				if managed(entry) {
					changed = true
					continue
				}
				next = append(next, entry)
			}
			if changed {
				hooks[event] = next
				result.HookRemoved = true
			}
		}
		if result.HookRemoved {
			if err := writeJSONAtomic(result.HookFile, settings); err != nil {
				return result, err
			}
		}
	}
	if options.PurgeConfig {
		if err := removeConfigFiles(result.ConfigFile, filepath.Join(home, ".cursor", "gtrace.json")); err != nil {
			return result, err
		}
		result.ConfigRemoved = true
	}
	if options.PurgeState {
		if err := os.RemoveAll(filepath.Join(home, ".cursor", "gtrace")); err != nil {
			return result, err
		}
		if err := removeFileIfExists(agentfiles.HookLogPath(home, "cursor")); err != nil {
			return result, err
		}
		result.StatePurged = true
	}
	return result, nil
}

func removeCodeBuddy(home string, options RemoveOptions) (RemoveResult, error) {
	result := RemoveResult{Adapter: "codebuddy", HookFile: filepath.Join(home, ".codebuddy", "settings.json"), ConfigFile: agentfiles.ConfigPath(home, "codebuddy")}
	settings, exists, err := readJSONObjectIfExists(result.HookFile)
	if err != nil {
		return result, err
	}
	if exists {
		hooks, _ := settings["hooks"].(map[string]any)
		managed := managedCodeBuddyHook
		if options.ConnectorOnly {
			managed = connectorManagedHook
		}
		for _, event := range []string{"Stop", "SessionEnd"} {
			groups, _ := hooks[event].([]any)
			next, changed := removeManagedGroups(groups, managed)
			if changed {
				hooks[event] = next
				result.HookRemoved = true
			}
		}
		if result.HookRemoved {
			if err := writeJSONAtomic(result.HookFile, settings); err != nil {
				return result, err
			}
		}
	}
	if options.PurgeConfig {
		if err := removeConfigFiles(result.ConfigFile, filepath.Join(home, ".codebuddy", "gtrace.json")); err != nil {
			return result, err
		}
		result.ConfigRemoved = true
	}
	if options.PurgeState {
		if err := os.RemoveAll(filepath.Join(home, ".codebuddy", "gtrace")); err != nil {
			return result, err
		}
		if err := removeFileIfExists(agentfiles.HookLogPath(home, "codebuddy")); err != nil {
			return result, err
		}
		result.StatePurged = true
	}
	return result, nil
}

func RemoveRuntime(home string) error {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}
	names := []string{"agent-telemetry", "gtrace-agent"}
	if runtime.GOOS == "windows" {
		names = []string{"agent-telemetry.exe", "gtrace-agent.exe"}
	}
	for _, name := range names {
		path := filepath.Join(home, ".local", "bin", name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func removeClaude(home string, options RemoveOptions) (RemoveResult, error) {
	result := RemoveResult{
		Adapter:    "claude",
		HookFile:   filepath.Join(home, ".claude", "settings.json"),
		ConfigFile: agentfiles.ConfigPath(home, "claude"),
	}
	settings, exists, err := readJSONObjectIfExists(result.HookFile)
	if err != nil {
		return result, err
	}
	if exists {
		hooks, _ := settings["hooks"].(map[string]any)
		managed := managedClaudeHook
		if options.ConnectorOnly {
			managed = connectorManagedHook
		}
		for _, event := range []string{"Stop", "SessionEnd"} {
			groups, _ := hooks[event].([]any)
			next, changed := removeManagedGroups(groups, managed)
			if changed {
				hooks[event] = next
				result.HookRemoved = true
			}
		}
		if result.HookRemoved {
			if err := writeJSONAtomic(result.HookFile, settings); err != nil {
				return result, err
			}
		}
	}
	if options.PurgeConfig {
		if err := removeConfigFiles(result.ConfigFile, filepath.Join(home, ".claude", "gtrace.json")); err != nil {
			return result, err
		}
		result.ConfigRemoved = true
	}
	if options.PurgeState {
		for _, name := range []string{"obs-agent-connector", "agent-telemetry", "gtrace-agent"} {
			if err := os.RemoveAll(filepath.Join(home, ".claude", "state", name)); err != nil {
				return result, err
			}
		}
		if err := removeFileIfExists(agentfiles.HookLogPath(home, "claude")); err != nil {
			return result, err
		}
		result.StatePurged = true
	}
	return result, nil
}

func removeCodex(home string, options RemoveOptions) (RemoveResult, error) {
	result := RemoveResult{
		Adapter:    "codex",
		HookFile:   filepath.Join(home, ".codex", "hooks.json"),
		ConfigFile: agentfiles.ConfigPath(home, "codex"),
	}
	settings, exists, err := readJSONObjectIfExists(result.HookFile)
	if err != nil {
		return result, err
	}
	managed := managedCodexHook
	if options.ConnectorOnly {
		managed = connectorManagedHook
	}
	var next []any
	var changed bool
	if exists {
		hooks, _ := settings["hooks"].(map[string]any)
		groups, _ := hooks["Stop"].([]any)
		locations := managedCodexTrustLocations(groups, managed)
		next, changed = removeManagedGroups(groups, managed)
		result.TrustRemoved, err = removeCodexTrustEntries(
			filepath.Join(home, ".codex", "config.toml"),
			result.HookFile,
			locations,
			len(next) == 0,
		)
		if err != nil {
			return result, fmt.Errorf("remove Codex Hook trust state: %w", err)
		}
		if changed {
			hooks["Stop"] = next
			result.HookRemoved = true
			if err := writeJSONAtomic(result.HookFile, settings); err != nil {
				return result, err
			}
		}
	} else {
		result.TrustRemoved, err = removeCodexTrustEntries(
			filepath.Join(home, ".codex", "config.toml"),
			result.HookFile,
			nil,
			true,
		)
		if err != nil {
			return result, fmt.Errorf("remove orphaned Codex Hook trust state: %w", err)
		}
	}
	if options.PurgeConfig {
		if err := removeConfigFiles(result.ConfigFile, filepath.Join(home, ".codex", "gtrace.json")); err != nil {
			return result, err
		}
		result.ConfigRemoved = true
	}
	if options.PurgeState {
		for _, name := range []string{"obs-agent-connector", "agent-telemetry", "gtrace-agent"} {
			if err := os.RemoveAll(filepath.Join(home, ".codex", "state", name)); err != nil {
				return result, err
			}
		}
		if err := removeFileIfExists(agentfiles.HookLogPath(home, "codex")); err != nil {
			return result, err
		}
		result.StatePurged = true
	}
	return result, nil
}

func removeConfigFiles(paths ...string) error {
	for _, path := range paths {
		if err := removeFileIfExists(path); err != nil {
			return err
		}
	}
	return nil
}

func managedCodexTrustLocations(groups []any, managed func(any) bool) map[string]struct{} {
	locations := map[string]struct{}{}
	for groupIndex, group := range groups {
		current, ok := group.(map[string]any)
		if !ok {
			continue
		}
		handlers, _ := current["hooks"].([]any)
		for handlerIndex, handler := range handlers {
			candidate := map[string]any{"hooks": []any{handler}}
			if managed(candidate) {
				locations[fmt.Sprintf("%d:%d", groupIndex, handlerIndex)] = struct{}{}
			}
		}
	}
	return locations
}

func removeCodexTrustEntries(path, hookFile string, locations map[string]struct{}, removeAllOrphans bool) (bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}

	lines := strings.SplitAfter(string(body), "\n")
	next := make([]string, 0, len(lines))
	removing := false
	changed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			removing = false
			if key, ok := codexTrustSectionKey(trimmed); ok && codexTrustKeyMatches(key, hookFile, locations, removeAllOrphans) {
				removing = true
				changed = true
				continue
			}
		}
		if !removing {
			next = append(next, line)
		}
	}
	if !changed {
		return false, nil
	}
	return true, writeTextAtomic(path, []byte(strings.Join(next, "")), info.Mode().Perm())
}

func codexTrustSectionKey(header string) (string, bool) {
	const prefix = "[hooks.state."
	if !strings.HasPrefix(header, prefix) || !strings.HasSuffix(header, "]") {
		return "", false
	}
	raw := strings.TrimSpace(header[len(prefix) : len(header)-1])
	if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return raw[1 : len(raw)-1], true
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return "", false
	}
	return value, true
}

func codexTrustKeyMatches(key, hookFile string, locations map[string]struct{}, removeAllOrphans bool) bool {
	normalized := strings.ReplaceAll(key, `\`, "/")
	marker := ":stop:"
	index := strings.LastIndex(strings.ToLower(normalized), marker)
	if index < 0 {
		return false
	}
	keyPath := filepath.Clean(normalized[:index])
	expectedPath := filepath.Clean(strings.ReplaceAll(hookFile, `\`, "/"))
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(keyPath, expectedPath) {
			return false
		}
	} else if keyPath != expectedPath {
		return false
	}
	if removeAllOrphans {
		return true
	}
	_, ok := locations[normalized[index+len(marker):]]
	return ok
}

func writeTextAtomic(path string, body []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".codex-config-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Rename(tempPath, path)
	}
	backup, err := os.CreateTemp(filepath.Dir(path), ".codex-config-backup-*.toml")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	defer os.Remove(backupPath)
	if err := os.Rename(path, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Rename(backupPath, path)
		return err
	}
	return os.Remove(backupPath)
}

func connectorManagedHook(value any) bool {
	group, ok := value.(map[string]any)
	if !ok {
		return false
	}
	handlers, _ := group["hooks"].([]any)
	for _, item := range handlers {
		handler, ok := item.(map[string]any)
		if !ok {
			continue
		}
		command := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fmt.Sprint(handler["command"])), `\`, "/"))
		if strings.Contains(command, "obs-agent-connector") {
			return true
		}
	}
	return false
}

func removeManagedGroups(groups []any, managed func(any) bool) ([]any, bool) {
	next := make([]any, 0, len(groups))
	changed := false
	for _, group := range groups {
		if managed(group) {
			changed = true
			continue
		}
		next = append(next, group)
	}
	return next, changed
}

func removeFileIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
