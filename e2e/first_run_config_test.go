package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFirstRunCreatesStarterConfig checks the first run without a config: hmj writes the
// starter config (mode 0600, in a new 0700 directory), tells the user to fill it in and exits
// with status 1. The next run starts from it and warns about the key variables it reads.
func TestFirstRunCreatesStarterConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.Config = filepath.Join(e.Home, ".hmj-first-run", "config.json") // its directory does not exist yet

	s := e.Start()
	s.Expect("created a starter config at " + e.Config)
	s.Expect("API key")
	if code := s.Wait(); code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	assertStarterConfig(t, e.Config)
	if fi, err := os.Stat(filepath.Dir(e.Config)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("config directory mode = %v (err %v), want 0700", fi.Mode().Perm(), err)
	}

	// Second run: the untouched starter config loads; cleanEnv unset both key variables.
	s = e.Start()
	s.Expect("config warning: provider gemini: apiKey refers to $GEMINI_API_KEY, which is not set")
	s.Expect("config warning: provider openai: apiKey refers to $OPENAI_API_KEY, which is not set")
	s.ExpectPrompt()
	s.Quit()
}

// TestEmptyConfigIsFilledWithStarter checks that an existing but empty config file is
// filled with the starter config and tightened to mode 0600.
func TestEmptyConfigIsFilledWithStarter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := os.WriteFile(e.Config, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := e.Start()
	s.Expect("created a starter config at " + e.Config)
	if code := s.Wait(); code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	assertStarterConfig(t, e.Config)
}

// TestInvalidConfigIsReportedNotReplaced checks that a config with broken JSON fails with an
// error naming the file, and is never overwritten by the starter config.
func TestInvalidConfigIsReportedNotReplaced(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.WriteConfig(`{"providers": {`)
	s := e.Start()
	s.Expect("config: " + e.Config)
	if code := s.Wait(); code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if got, _ := os.ReadFile(e.Config); string(got) != `{"providers": {` {
		t.Errorf("broken config was changed to %q", got)
	}
}

// assertStarterConfig checks that path holds config.example.json with mode 0600.
func assertStarterConfig(t *testing.T, path string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("starter config not written: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("starter config differs from config.example.json:\n%s", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("starter config mode = %v, want 0600", fi.Mode().Perm())
	}
}
