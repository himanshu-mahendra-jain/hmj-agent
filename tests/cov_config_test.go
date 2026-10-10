package tests

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/config"
)

// TestStarterConfigFirstRunFlow writes the starter template on a first run and checks that it
// loads, has no providers (so a hot reload keeps the current config), and is never overwritten.
func TestStarterConfigFirstRunFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hmj", "config.json")
	template := []byte(`{"providers":{}}`)

	if err := config.CreateStarterConfig(path, template); err != nil {
		t.Fatalf("CreateStarterConfig: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("starter file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(starter): %v", err)
	}
	if _, err := config.ReloadConfig(path, cfg); err == nil || !strings.Contains(err.Error(), "no providers") {
		t.Errorf("ReloadConfig(empty starter) error = %v, want 'no providers configured'", err)
	}
	if _, err := cfg.DefaultModel(); !errors.Is(err, config.ErrNoModels) {
		t.Errorf("DefaultModel on starter = %v, want ErrNoModels", err)
	}

	// The user fills the file in; a second first-run call must leave it alone.
	mine := []byte(`{"providers":{"p":{"baseUrl":"https://x"}}}`)
	if err := os.WriteFile(path, mine, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.CreateStarterConfig(path, template); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CreateStarterConfig over content: %v, want fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(mine) {
		t.Errorf("user config overwritten: %s", got)
	}
}

// TestEnvKeyReferenceEndToEnd checks a $VAR apiKey: it resolves from the environment, is
// reported when unset, and is never printed in clear.
func TestEnvKeyReferenceEndToEnd(t *testing.T) {
	t.Setenv("HMJ_COV_E2E_KEY", "sk-test-value")
	t.Setenv("HMJ_COV_E2E_MISSING", "")
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"providers":{
		"set":{"baseUrl":"https://api.example.com/v1","apiKey":"$HMJ_COV_E2E_KEY","models":[{"id":"m"}]},
		"unset":{"baseUrl":"https://api.example.com/v1","apiKey":"$HMJ_COV_E2E_MISSING","models":[{"id":"n"}]}
	}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	p, err := cfg.Resolve(config.ModelRef{Provider: "set", Model: "m"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := config.ResolveAPIKey(p.APIKey); got != "sk-test-value" {
		t.Errorf("resolved key = %q", got)
	}
	if s := p.APIKey.String(); strings.Contains(s, "sk-test") || strings.Contains(s, "HMJ_COV") {
		t.Errorf("key printed in clear: %q", s)
	}
	notes := config.UnsetKeyVars(cfg)
	if len(notes) != 1 || !strings.Contains(notes[0], "provider unset") {
		t.Errorf("UnsetKeyVars = %v, want one note for provider unset", notes)
	}
	for _, w := range config.SecurityWarnings(path, cfg) {
		if strings.Contains(w, "sk-test") {
			t.Errorf("warning leaks key: %s", w)
		}
	}
}
