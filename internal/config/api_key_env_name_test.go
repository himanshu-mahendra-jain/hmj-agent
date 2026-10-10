package config

import "testing"

// TestAPIKeyEnvName checks that only a $NAME value names an environment variable.
func TestAPIKeyEnvName(t *testing.T) {
	for value, want := range map[Secret]string{
		"$OPENROUTER_KEY": "OPENROUTER_KEY",
		"sk-literal":      "",
		"":                "",
		"$":               "",
	} {
		if got := APIKeyEnvName(value); got != want {
			t.Errorf("APIKeyEnvName(%q) = %q, want %q", string(value), got, want)
		}
	}
}
