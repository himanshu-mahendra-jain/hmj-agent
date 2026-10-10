package redact

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"openai key", "key=sk-proj-abcdefghijklmnopqrstuvwx123 ok", "key=[REDACTED] ok"},
		{"anthropic key", "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV", "[REDACTED]"},
		{"google key", "AIzaSyA1234567890abcdefghijklmnopqrstuv", "[REDACTED]"},
		{"github token", "ghp_" + strings.Repeat("a1", 18), "[REDACTED]"},
		{"aws key id", "id AKIAIOSFODNN7EXAMPLE end", "id [REDACTED] end"},
		{"bearer header", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig", "Authorization: Bearer [REDACTED]"},
		{"private key", "a\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn\nNzaC1\n-----END OPENSSH PRIVATE KEY-----\nb", "a\n[REDACTED]\nb"},
		{"prose untouched", "bearer tokens and task-runner-configuration-file-name", "bearer tokens and task-runner-configuration-file-name"},
		{"short sk untouched", "sk-short", "sk-short"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := String(tc.in); got != tc.want {
				t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRegisteredSecrets(t *testing.T) {
	Register("custom-provider-secret-42", "short", `quote"secret-value`)
	got := String(`token custom-provider-secret-42 and short and quote"secret-value`)
	if want := `token [REDACTED] and short and quote"secret-value`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Redacting encoded JSON leaves it valid and decodes to the redacted text.
func TestBytesKeepsJSONValid(t *testing.T) {
	in := map[string]string{"content": "line\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\"x\\y\n-----END RSA PRIVATE KEY-----\nkey sk-abcdefghijklmnopqrstuvwxyz"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(Bytes(data), &out); err != nil {
		t.Fatalf("redacted JSON is invalid: %v", err)
	}
	if want := "line\n[REDACTED]\nkey [REDACTED]"; out["content"] != want {
		t.Errorf("got %q, want %q", out["content"], want)
	}
}
