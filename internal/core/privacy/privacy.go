package privacy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxDepth = 8
	maxItems = 100
)

var (
	secretKey           = regexp.MustCompile(`(?i)(authorization|cookie|password|passwd|secret|token|api[-_]?key|access[-_]?key|private[-_]?key|credential)`)
	authorizationLine   = regexp.MustCompile(`(?im)\b(authorization|cookie)\s*:\s*[^\r\n]+`)
	assignedSecret      = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[-_ ]?key|access[-_ ]?key)\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)
	bearer              = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	keyValue            = regexp.MustCompile(`(?i)\b(sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9_]{12,})\b`)
	privateKeyBlock     = regexp.MustCompile(`(?is)-----BEGIN[ \t]+(?:[A-Z0-9]+[ \t]+)*PRIVATE[ \t]+KEY-----.*?-----END[ \t]+(?:[A-Z0-9]+[ \t]+)*PRIVATE[ \t]+KEY-----`)
	privateKeyRemainder = regexp.MustCompile(`(?is)-----BEGIN[ \t]+(?:[A-Z0-9]+[ \t]+)*PRIVATE[ \t]+KEY-----.*`)
)

func Sanitize(value any, maxChars int) any {
	if maxChars <= 0 {
		maxChars = 20_000
	}
	return sanitize(value, "", maxChars, 0)
}

func Text(value any, maxChars int) string {
	sanitized := Sanitize(value, maxChars)
	var text string
	switch current := sanitized.(type) {
	case string:
		text = current
	default:
		body, err := json.Marshal(current)
		if err != nil {
			text = fmt.Sprint(current)
		} else {
			text = string(body)
		}
	}
	return clip(text, maxChars)
}

func Preview(value any, maxChars int) string {
	return strings.Join(strings.Fields(Text(value, maxChars)), " ")
}

func sanitize(value any, key string, maxChars, depth int) any {
	if secretKey.MatchString(key) {
		return "[REDACTED]"
	}
	if depth >= maxDepth {
		return "[TRUNCATED_DEPTH]"
	}
	switch current := value.(type) {
	case nil:
		return nil
	case string:
		return clip(redactText(current), maxChars)
	case bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return current
	case map[string]any:
		out := make(map[string]any, min(len(current), maxItems))
		count := 0
		for childKey, child := range current {
			if count >= maxItems {
				out["_truncated_items"] = len(current) - count
				break
			}
			out[childKey] = sanitize(child, childKey, maxChars, depth+1)
			count++
		}
		return out
	case []any:
		size := min(len(current), maxItems)
		out := make([]any, 0, size+1)
		for index := 0; index < size; index++ {
			out = append(out, sanitize(current[index], key, maxChars, depth+1))
		}
		if len(current) > size {
			out = append(out, fmt.Sprintf("[TRUNCATED_%d_ITEMS]", len(current)-size))
		}
		return out
	default:
		body, err := json.Marshal(current)
		if err != nil {
			return clip(redactText(fmt.Sprint(current)), maxChars)
		}
		var normalized any
		if err := json.Unmarshal(body, &normalized); err != nil {
			return clip(redactText(string(body)), maxChars)
		}
		return sanitize(normalized, key, maxChars, depth+1)
	}
}

func redactText(value string) string {
	value = privateKeyBlock.ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
	value = privateKeyRemainder.ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
	value = authorizationLine.ReplaceAllString(value, "$1: [REDACTED]")
	value = assignedSecret.ReplaceAllString(value, "$1=[REDACTED]")
	value = bearer.ReplaceAllString(value, "Bearer [REDACTED]")
	return keyValue.ReplaceAllString(value, "[REDACTED]")
}

func clip(value string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(value) <= maxChars {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxChars])
}
