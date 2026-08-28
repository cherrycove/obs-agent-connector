package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GuanceCloud/obs-agent-connector/internal/core/agentfiles"
	"github.com/GuanceCloud/obs-agent-connector/internal/core/transport"
)

const (
	DefaultMaxChars  = 20_000
	DefaultTimeoutMs = 10_000
)

type Config struct {
	Enabled            bool
	Transport          transport.Config
	ResourceAttributes map[string]any
	CaptureContent     string
	MaxChars           int
	Debug              bool
	LogFile            string
	StateDir           string
}

type ResolveOptions struct {
	Env  map[string]string
	Home string
	Cwd  string
}

func Resolve(options ResolveOptions) Config {
	env := options.Env
	if env == nil {
		env = environment()
	}
	home := options.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	merged := map[string]any{
		"tracePath": "v1/traces", "metricsPath": "v1/metrics",
		"captureContent": "preview", "maxChars": DefaultMaxChars,
		"timeoutMs": DefaultTimeoutMs, "headers": map[string]any{},
		"resourceAttributes": map[string]any{
			"service.name": "gtrace-grok", "telemetry.sdk.name": "gtrace",
			"telemetry.sdk.language": "go", "agent_runtime": "grok",
			"agent_name": "Grok Build",
		},
	}
	for _, source := range []map[string]any{
		readJSON(agentfiles.ConfigPath(home, "grok")),
		environmentConfig(env),
	} {
		merge(merged, source)
	}

	endpoint := firstString(merged, "endpoint", "base_url")
	tracePath := normalizedPath(firstString(merged, "tracePath", "trace_path"), "v1/traces")
	metricsPath := normalizedPath(firstString(merged, "metricsPath", "metrics_path"), "v1/metrics")
	if metricsPath == "v1/metrics" && tracePath == "v1/write/otel-llm" {
		metricsPath = "v1/write/otel-metrics"
	}
	traceURL := firstString(merged, "otel_traces_url", "tracesEndpoint", "traceEndpoint")
	metricsURL := firstString(merged, "otel_metrics_url", "metricsEndpoint", "metricEndpoint")
	enabled, hasEnabled := boolean(merged["enabled"])
	if !hasEnabled {
		enabled = endpoint != "" || traceURL != "" || metricsURL != ""
	}
	capture := strings.ToLower(firstString(merged, "captureContent", "capture_content"))
	if capture != "none" && capture != "full" {
		capture = "preview"
	}
	stateDir := expandPath(firstString(merged, "stateDir", "state_dir"), home)
	if stateDir == "" {
		stateDir = filepath.Join(agentfiles.Directory(home, "grok"), "state")
	}
	return Config{
		Enabled: enabled,
		Transport: transport.Config{
			Endpoint: strings.TrimRight(endpoint, "/"), TracePath: tracePath, MetricsPath: metricsPath,
			TraceURL: strings.TrimRight(traceURL, "/"), MetricsURL: strings.TrimRight(metricsURL, "/"),
			Headers: stringMap(merged["headers"]), PublicKey: firstString(merged, "public_key", "publicKey"),
			SecretKey: firstString(merged, "secret_key", "secretKey"),
			Timeout:   time.Duration(integer(merged["timeoutMs"], integer(merged["timeout_ms"], DefaultTimeoutMs))) * time.Millisecond,
		},
		ResourceAttributes: primitiveMap(merged["resourceAttributes"]),
		CaptureContent:     capture,
		MaxChars:           bounded(integer(merged["maxChars"], integer(merged["max_chars"], DefaultMaxChars)), 1, 100_000, DefaultMaxChars),
		Debug:              boolValue(merged["debug"]),
		LogFile:            agentfiles.HookLogPath(home, "grok"),
		StateDir:           stateDir,
	}
}

func environmentConfig(env map[string]string) map[string]any {
	return clean(map[string]any{
		"enabled":            firstEnv(env, "GROK_OTEL_ENABLED", "TRACE_TO_GTRACE"),
		"endpoint":           firstEnv(env, "OTEL_EXPORTER_OTLP_ENDPOINT", "GROK_OTEL_ENDPOINT", "GTRACE_ENDPOINT"),
		"otel_traces_url":    firstEnv(env, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "GROK_OTEL_TRACES_ENDPOINT"),
		"otel_metrics_url":   firstEnv(env, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "GROK_OTEL_METRICS_ENDPOINT"),
		"tracePath":          firstEnv(env, "GROK_OTEL_TRACE_PATH", "GTRACE_TRACE_PATH"),
		"metricsPath":        firstEnv(env, "GROK_OTEL_METRICS_PATH", "GTRACE_METRICS_PATH"),
		"headers":            parseObject(firstEnv(env, "OTEL_EXPORTER_OTLP_HEADERS", "GROK_OTEL_HEADERS")),
		"resourceAttributes": parseObject(firstEnv(env, "OTEL_RESOURCE_ATTRIBUTES", "GROK_OTEL_RESOURCE_ATTRIBUTES")),
		"captureContent":     firstEnv(env, "GROK_OTEL_CAPTURE_CONTENT", "GTRACE_CAPTURE_CONTENT"),
		"maxChars":           firstEnv(env, "GROK_OTEL_MAX_CHARS", "GTRACE_MAX_CHARS"),
		"timeoutMs":          firstEnv(env, "GROK_OTEL_TIMEOUT_MS", "GTRACE_TIMEOUT_MS"),
		"debug":              firstEnv(env, "GROK_OTEL_DEBUG", "GTRACE_DEBUG"),
		"stateDir":           firstEnv(env, "GROK_OTEL_STATE_DIR"),
	})
}

func readJSON(path string) map[string]any {
	body, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return map[string]any{}
	}
	return value
}

func merge(target, source map[string]any) {
	for key, value := range source {
		if value == nil || value == "" {
			continue
		}
		if key == "headers" || key == "resourceAttributes" {
			current := objectMap(target[key])
			for childKey, childValue := range objectMap(value) {
				current[childKey] = childValue
			}
			target[key] = current
			continue
		}
		target[key] = value
	}
}

func environment() map[string]string {
	out := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			out[key] = value
		}
	}
	return out
}

func firstEnv(env map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(env[name]); value != "" {
			return value
		}
	}
	return ""
}

func firstString(values map[string]any, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(toString(values[name])); value != "" {
			return value
		}
	}
	return ""
}

func toString(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case bool:
		return strconv.FormatBool(current)
	case float64:
		return strconv.FormatFloat(current, 'f', -1, 64)
	case json.Number:
		return current.String()
	}
	return ""
}

func objectMap(value any) map[string]any {
	out := map[string]any{}
	if current, ok := value.(map[string]any); ok {
		for key, item := range current {
			out[key] = item
		}
	}
	return out
}

func stringMap(value any) map[string]string {
	out := map[string]string{}
	for key, item := range objectMap(value) {
		if text := strings.TrimSpace(toString(item)); text != "" {
			out[key] = text
		}
	}
	return out
}

func primitiveMap(value any) map[string]any {
	out := map[string]any{}
	for key, item := range objectMap(value) {
		switch item.(type) {
		case string, bool, float64, int, int64:
			out[key] = item
		}
	}
	return out
}

func parseObject(value string) map[string]any {
	out := map[string]any{}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "{") {
		_ = json.Unmarshal([]byte(value), &out)
		return out
	}
	for _, entry := range strings.Split(value, ",") {
		if key, item, ok := strings.Cut(entry, "="); ok && strings.TrimSpace(key) != "" && strings.TrimSpace(item) != "" {
			out[strings.TrimSpace(key)] = strings.TrimSpace(item)
		}
	}
	return out
}

func clean(value map[string]any) map[string]any {
	for key, item := range value {
		if item == nil || item == "" {
			delete(value, key)
		}
	}
	return value
}

func integer(value any, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(toString(value)))
	if err == nil {
		return parsed
	}
	return fallback
}

func boolean(value any) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(toString(value))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

func boolValue(value any) bool {
	result, _ := boolean(value)
	return result
}

func normalizedPath(value, fallback string) string {
	if value = strings.Trim(strings.TrimSpace(value), "/"); value != "" {
		return value
	}
	return fallback
}

func bounded(value, minimum, maximum, fallback int) int {
	if value < minimum || value > maximum {
		return fallback
	}
	return value
}

func expandPath(value, home string) string {
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	return value
}
