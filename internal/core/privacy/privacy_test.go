package privacy

import (
	"strings"
	"testing"
)

func TestSanitizeRedactsSecretKeysAndBearerValues(t *testing.T) {
	value := Sanitize(map[string]any{
		"authorization": "Bearer secret-value",
		"nested": map[string]any{
			"message": "use Bearer abcdefghijklmno",
			"token":   "top-secret",
		},
	}, 100)
	text := Text(value, 1000)
	if strings.Contains(text, "secret-value") || strings.Contains(text, "abcdefghijklmno") || strings.Contains(text, "top-secret") {
		t.Fatalf("secret leaked: %s", text)
	}
}

func TestTextClipsByRunes(t *testing.T) {
	if got := Text("Καλημέρα", 4); got != "Καλη" {
		t.Fatalf("unexpected clipped text %q", got)
	}
}

func TestSanitizeRedactsEmbeddedHeadersAssignmentsAndPrivateKeys(t *testing.T) {
	value := Text(map[string]any{
		"message": "Authorization: Basic dXNlcjpwYXNz\nCookie: session=hidden\npassword=plain-secret api_key='api-secret'\n-----BEGIN OPENSSH PRIVATE KEY-----\nprivate-material\n-----END OPENSSH PRIVATE KEY-----\nafter",
	}, 10_000)
	for _, secret := range []string{"dXNlcjpwYXNz", "session=hidden", "plain-secret", "api-secret", "private-material"} {
		if strings.Contains(value, secret) {
			t.Fatalf("embedded secret %q leaked: %s", secret, value)
		}
	}
	if !strings.Contains(value, "after") {
		t.Fatalf("redacting a complete private key removed following text: %s", value)
	}

	incomplete := Text("before -----BEGIN PRIVATE KEY-----\npartial-secret", 10_000)
	if strings.Contains(incomplete, "partial-secret") {
		t.Fatalf("incomplete private key leaked: %s", incomplete)
	}
}
