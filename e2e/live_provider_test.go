package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveProvider runs one real tool-using turn against a real provider, to catch what a
// mock cannot: wire-format drift, a provider that buffers its stream or reports no usage,
// reasoning round-trips. It is skipped unless enabled:
//
//	HMJ_E2E_LIVE_CONFIG=~/.hmj/config.json HMJ_E2E_LIVE_MODEL=provider/model make e2e-live
//
// The config is copied into the test's own directory, so the real sessions, audit log and
// backups are not touched. "$ENV_VAR" API keys are passed through from the environment.
func TestLiveProvider(t *testing.T) {
	cfgPath, model := os.Getenv("HMJ_E2E_LIVE_CONFIG"), os.Getenv("HMJ_E2E_LIVE_MODEL")
	if cfgPath == "" || model == "" {
		t.Skip("set HMJ_E2E_LIVE_CONFIG and HMJ_E2E_LIVE_MODEL (provider/model) to run against a real provider")
	}
	if strings.HasPrefix(cfgPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		cfgPath = filepath.Join(home, cfgPath[2:])
	}
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	e := newEnv(t)
	e.WriteConfig(string(cfg))
	e.WriteFile("notes.txt", "alpha\nbravo-7291\ncharlie\n")
	var keys []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_TOKEN") {
			keys = append(keys, kv)
		}
	}
	s := e.StartWithEnv(keys)
	s.timeout = 3 * time.Minute
	s.ExpectPrompt()

	s.Type("\x0f") // Ctrl+O
	s.Expect("select model")
	s.TypeSlowly(model)
	s.Expect("1/1 matches")
	s.Type("\r")
	s.Expect("model: " + model)
	s.ExpectPrompt()

	s.Type("Use the read_file tool to read notes.txt, then reply with only its second line.\r")
	s.Expect("read_file: notes.txt")
	s.Expect("bravo-7291")
	s.Expect("[tokens:")
	s.ExpectPrompt()
	if footer := s.Screen(); strings.Contains(footer, "[tokens: 0 input | 0 output") {
		t.Log("note: the provider reported no token usage")
	}
	s.Quit()
}
