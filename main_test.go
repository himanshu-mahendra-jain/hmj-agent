package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
	"hmj-agent/internal/tools"
)

func TestRunFlags(t *testing.T) {
	// Test version flags
	for _, flag := range []string{"--version", "-v", "version"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flag}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit code 0 for %s, got %d", flag, code)
		}
		if !strings.Contains(stdout.String(), config.Version) {
			t.Fatalf("expected %s in stdout for %s, got %q", config.Version, flag, stdout.String())
		}
	}

	// Test help / shortcuts flags
	for _, flag := range []string{"--help", "-h", "help", "/shortcuts", "shortcuts", "--shortcuts"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flag}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit code 0 for %s, got %d", flag, code)
		}
	}

	// Test accessible and no-color flags
	theme.SetAccessibility(false)
	theme.SetEnabled(true)

	var stdout, stderr bytes.Buffer
	// Invalid config path by pointing HOME to tempdir
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	code := run([]string{"--accessible", "--no-color"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 when config missing, got %d", code)
	}
	if !theme.Accessibility() {
		t.Fatal("expected accessibility to be set to true")
	}
	if theme.Enabled() {
		t.Fatal("expected theme to be disabled with --no-color")
	}
}

func TestRunConfigValidation(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	configDir := filepath.Join(tmpHome, ".hmj")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Config with syntax error
	badConfig := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(badConfig, []byte("{invalid-json"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 on bad json, got %d", code)
	}

	// 2. Config with 0 providers
	if err := os.WriteFile(badConfig, []byte(`{"providers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	code = run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 on 0 providers, got %d", code)
	}
	if !strings.Contains(stderr.String(), "no providers configured") {
		t.Fatalf("expected 'no providers configured' in stderr, got %q", stderr.String())
	}

	// 3. Config with provider but invalid default model
	noModelConfig := `{
		"providers": {
			"prov1": {
				"apiKey": "k",
				"models": []
			}
		}
	}`
	if err := os.WriteFile(badConfig, []byte(noModelConfig), 0600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	code = run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1 when no models available, got %d", code)
	}

	// 4. Valid config with immediate EOF on stdin
	validConfig := `{
		"accessibility": true,
		"providers": {
			"prov1": {
				"baseUrl": "https://api.test.com/v1",
				"apiKey": "k",
				"models": [
					{ "id": "m1", "name": "Model 1" }
				]
			}
		}
	}`
	if err := os.WriteFile(badConfig, []byte(validConfig), 0600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	stdout.Reset()
	// Pass an EOF reader
	code = run(nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on clean EOF, got %d (stderr: %s)", code, stderr.String())
	}
}

func TestRunInteractiveShortcuts(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	configDir := filepath.Join(tmpHome, ".hmj")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	validConfig := fmt.Sprintf(`{
		"providers": {
			"prov1": {
				"baseUrl": "%s",
				"apiKey": "k",
				"models": [
					{ "id": "m1", "name": "Model 1" }
				]
			}
		}
	}`, server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(validConfig), 0600); err != nil {
		t.Fatal(err)
	}

	// Feed shortcut commands and prompts
	inputs := "/shortcuts\n"
	var stdout, stderr bytes.Buffer
	code := run(nil, strings.NewReader(inputs), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on shortcut inputs, got %d", code)
	}

	// Test action shortcuts: /new, prompt
	inputs = "/new\n"
	code = run(nil, strings.NewReader(inputs), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on /new, got %d", code)
	}

	inputs = "hello\n"
	code = run(nil, strings.NewReader(inputs), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on hello, got %d", code)
	}

	// Test history command when history has messages
	inputs = "/history\n"
	code = run(nil, strings.NewReader(inputs), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on /history, got %d", code)
	}

	// Test /resume and /models commands
	inputs = "/resume\n\n"
	code = run(nil, strings.NewReader(inputs), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on /resume, got %d", code)
	}

	inputs = "/models\n\n"
	code = run(nil, strings.NewReader(inputs), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on /models, got %d", code)
	}

	// Test /health command
	inputs = "/health\n"
	code = run(nil, strings.NewReader(inputs), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0 on /health, got %d", code)
	}

	// Test control key action shortcuts: \x0e (new), \x19 (history), \x0f (models), \x12 (resume)
	for _, act := range []string{"\x0e\n", "\x19\n\n", "\x0f\n\n", "\x12\n\n"} {
		code = run(nil, strings.NewReader(act), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit code 0 on action %q, got %d", act, code)
		}
	}
}

func TestRunHealthFlag(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"delta\":\"1\"}\n\n")
	}))
	defer server.Close()

	configDir := filepath.Join(tmpHome, ".hmj")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{
		"providers": {
			"prov1": {
				"baseUrl": "%s",
				"apiKey": "k",
				"models": [
					{ "id": "m1", "name": "Model 1" }
				]
			}
		}
	}`, server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	for _, flag := range []string{"--health", "health"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flag}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("expected exit code 0 for flag %s, got %d (stderr: %s)", flag, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Health Check") {
			t.Fatalf("expected health check output for %s, got: %s", flag, stdout.String())
		}
	}
}

// applyDisplayConfig: an explicit flag wins over the config, and a reload can switch a mode
// off again when neither the flag nor the config asks for it.
func TestDisplayModePrecedence(t *testing.T) {
	defer theme.SetAccessibility(false)
	defer theme.SetHighContrast(false)
	t.Setenv("HIGH_CONTRAST", "")
	t.Setenv("ACCESSIBILITY", "")

	tests := []struct {
		name       string
		forced     forcedFlags
		cfg        config.Config
		wantAccess bool
		wantHC     bool
	}{
		{"defaults off", forcedFlags{}, config.Config{}, false, false},
		{"config turns both on", forcedFlags{}, config.Config{Accessibility: true, HighContrast: true}, true, true},
		{"flags win over config off", forcedFlags{accessible: true, highContrast: true}, config.Config{}, true, true},
		{"config high contrast only", forcedFlags{}, config.Config{HighContrast: true}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyDisplayConfig(tt.forced, tt.cfg)
			if theme.Accessibility() != tt.wantAccess || theme.HighContrast() != tt.wantHC {
				t.Errorf("accessibility=%v highContrast=%v, want %v and %v",
					theme.Accessibility(), theme.HighContrast(), tt.wantAccess, tt.wantHC)
			}
		})
	}

	// A reload that drops the config keys switches the modes back off, unless a flag holds them.
	applyDisplayConfig(forcedFlags{}, config.Config{})
	if theme.Accessibility() || theme.HighContrast() {
		t.Error("reload without flags or config should turn both modes off")
	}
	applyDisplayConfig(forcedFlags{highContrast: true}, config.Config{})
	if !theme.HighContrast() {
		t.Error("--high-contrast flag must survive a reload that drops the config key")
	}
}

func TestHighContrastFlag(t *testing.T) {
	defer theme.SetHighContrast(false)
	t.Setenv("HIGH_CONTRAST", "")

	var stderr bytes.Buffer
	forced, code, exit := applyFlags([]string{"--high-contrast"}, io.Discard, &stderr)
	if exit || code != 0 {
		t.Fatalf("applyFlags exit=%v code=%d, want to continue", exit, code)
	}
	if !forced.highContrast || !theme.HighContrast() {
		t.Errorf("--high-contrast not applied: forced=%+v HighContrast=%v", forced, theme.HighContrast())
	}
}

// A missing config that cannot be created (read-only directory) fails, and in accessibility
// mode prints the remediation hint on its own line.
func TestRunMissingConfigHintInAccessibleMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	defer theme.SetAccessibility(false)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	t.Setenv("HMJ_CONFIG", filepath.Join(dir, "config.json"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"--accessible"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "\n[HINT] create ~/.hmj/config.json (see config.example.json) or set HMJ_CONFIG\n") {
		t.Errorf("expected hint line in stderr, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "creating a starter config failed") {
		t.Errorf("expected the failed starter-config creation in stderr, got %q", stderr.String())
	}
}

func TestSignalExitCode(t *testing.T) {
	tests := []struct {
		sig  os.Signal
		want int
	}{
		{syscall.SIGHUP, 129},
		{syscall.SIGQUIT, 131},
		{syscall.SIGINT, 130},
		{syscall.SIGTERM, 143},
	}
	for _, tc := range tests {
		if got := signalExitCode(tc.sig); got != tc.want {
			t.Errorf("signalExitCode(%v) = %d, want %d", tc.sig, got, tc.want)
		}
	}
}

const reloadTestConfig = `{"providers":{"%s":{"baseUrl":"http://127.0.0.1:1","apiKey":"k","models":[{"id":"%s"}]}}%s}`

// newReloadApp writes a config with provider "alpha" and returns an app that loaded it.
func newReloadApp(t *testing.T, forced forcedFlags) *app {
	t.Helper()
	t.Setenv("HIGH_CONTRAST", "")
	t.Setenv("ACCESSIBILITY", "")
	t.Cleanup(func() { theme.SetAccessibility(false); theme.SetHighContrast(false) })
	path := filepath.Join(t.TempDir(), "config.json")
	writeReloadConfig(t, path, "alpha", "m1", "")
	t.Setenv("HMJ_CONFIG", path)
	a, err := loadApp(forced)
	if err != nil {
		t.Fatal(err)
	}
	a.stampConfigLoaded()
	return a
}

func writeReloadConfig(t *testing.T, path, provider, model, extra string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fmt.Sprintf(reloadTestConfig, provider, model, extra)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = old; f.Close() }()
	fn()
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

func bumpMTime(t *testing.T, path string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func TestReloadConfigUnchangedFileIsSkipped(t *testing.T) {
	a := newReloadApp(t, forcedFlags{})
	// Same size and modification time but different content: only the stamp is consulted.
	stat, _ := os.Stat(a.configPath)
	writeReloadConfig(t, a.configPath, "omega", "m1", "")
	if err := os.Chtimes(a.configPath, stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}
	a.reloadConfig()
	if _, ok := a.cfg.Providers["alpha"]; !ok || len(a.cfg.Providers) != 1 {
		t.Errorf("config was re-read although mtime and size match: %v", a.cfg.Providers)
	}
}

func TestReloadConfigSwapsConfigClientAndModel(t *testing.T) {
	a := newReloadApp(t, forcedFlags{})
	if a.modelRef != (config.ModelRef{Provider: "alpha", Model: "m1"}) {
		t.Fatalf("initial model = %v", a.modelRef)
	}
	writeReloadConfig(t, a.configPath, "beta", "m2", "")
	bumpMTime(t, a.configPath)

	if out := captureStderr(t, a.reloadConfig); out != "" {
		t.Errorf("successful reload printed %q", out)
	}
	if _, ok := a.cfg.Providers["beta"]; !ok {
		t.Fatalf("cfg not swapped: %v", a.cfg.Providers)
	}
	if _, ok := a.client.Config.Providers["beta"]; !ok {
		t.Error("client config not swapped")
	}
	if a.modelRef != (config.ModelRef{Provider: "beta", Model: "m2"}) {
		t.Errorf("default model = %v, want beta/m2", a.modelRef)
	}
	stat, _ := os.Stat(a.configPath)
	if !a.cfgModTime.Equal(stat.ModTime()) || a.cfgSize != stat.Size() {
		t.Errorf("stamp not refreshed after reload: %v/%d vs %v/%d", a.cfgModTime, a.cfgSize, stat.ModTime(), stat.Size())
	}
}

func TestReloadConfigInvalidKeepsOldAndWarns(t *testing.T) {
	a := newReloadApp(t, forcedFlags{})
	for name, content := range map[string]string{
		"malformed json": `{"providers": `,
		"no providers":   `{"providers":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(a.configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			bumpMTime(t, a.configPath)
			out := captureStderr(t, a.reloadConfig)
			if !strings.Contains(out, "[warning: config reload failed:]") {
				t.Errorf("stderr = %q, want reload warning", out)
			}
			if _, ok := a.cfg.Providers["alpha"]; !ok {
				t.Error("old config was lost")
			}
			if _, ok := a.client.Config.Providers["alpha"]; !ok {
				t.Error("client config was replaced by an invalid one")
			}
			if a.modelRef.Provider != "alpha" {
				t.Errorf("model changed to %v", a.modelRef)
			}
		})
	}
}

func TestReloadConfigDeletedFileIsSilent(t *testing.T) {
	a := newReloadApp(t, forcedFlags{})
	if err := os.Remove(a.configPath); err != nil {
		t.Fatal(err)
	}
	if out := captureStderr(t, a.reloadConfig); out != "" {
		t.Errorf("deleted config printed %q, want silence", out)
	}
	if _, ok := a.cfg.Providers["alpha"]; !ok {
		t.Error("config dropped after the file was deleted")
	}
}

func TestReloadConfigDisplayModes(t *testing.T) {
	t.Run("config turns modes on and off", func(t *testing.T) {
		a := newReloadApp(t, forcedFlags{})
		writeReloadConfig(t, a.configPath, "alpha", "m1", `,"accessibility":true,"highContrast":true`)
		bumpMTime(t, a.configPath)
		a.reloadConfig()
		if !theme.Accessibility() || !theme.HighContrast() {
			t.Fatalf("modes after reload: accessibility=%v highContrast=%v, want both on", theme.Accessibility(), theme.HighContrast())
		}
		writeReloadConfig(t, a.configPath, "alpha", "m1", "")
		later := time.Now().Add(2 * time.Hour)
		os.Chtimes(a.configPath, later, later)
		a.reloadConfig()
		if theme.Accessibility() || theme.HighContrast() {
			t.Error("modes should switch off when the config drops them")
		}
	})
	t.Run("command-line flags win", func(t *testing.T) {
		a := newReloadApp(t, forcedFlags{accessible: true, highContrast: true})
		writeReloadConfig(t, a.configPath, "alpha", "m1", `,"accessibility":false,"highContrast":false`)
		bumpMTime(t, a.configPath)
		a.reloadConfig()
		if !theme.Accessibility() || !theme.HighContrast() {
			t.Errorf("flags lost on reload: accessibility=%v highContrast=%v", theme.Accessibility(), theme.HighContrast())
		}
	})
}

// TestRunHealthMissingConfigCreatesStarter checks that a first run without a config writes the
// starter config (mode 0600) and tells the user to fill it in, instead of only failing.
func TestRunHealthMissingConfigCreatesStarter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HMJ_CONFIG", "")
	var stdout, stderr bytes.Buffer
	if code := runHealth(&stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	path := filepath.Join(home, ".hmj", "config.json")
	if !strings.Contains(stderr.String(), "created a starter config at "+path) || !strings.Contains(stderr.String(), "API key") {
		t.Errorf("stderr = %q, want the starter-config notice", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("starter config not written: %v", err)
	}
	if !bytes.Equal(data, starterConfig) {
		t.Errorf("starter config content differs from config.example.json")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("starter config mode = %v, want 0600", fi.Mode().Perm())
	}

	// The second run loads the starter config instead of recreating it.
	stderr.Reset()
	if _, err := loadConfig(path); err != nil {
		t.Errorf("starter config does not load: %v", err)
	}
}

func TestRunHealthHealthyProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"delta\":\"1\"}\n\n")
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{"providers":{"prov1":{"baseUrl":%q,"apiKey":"k","models":[{"id":"m1","name":"Model 1"}]}}}`, server.URL)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HMJ_CONFIG", path)

	var stdout, stderr bytes.Buffer
	if code := runHealth(&stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Health Check") || !strings.Contains(stdout.String(), "m1") {
		t.Errorf("stdout = %q, want a health table naming the model", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// A save that keeps failing (e.g. a read-only filesystem) is reported once, not on
// every turn; recovery is announced; the failed save leaves no temp file behind.
func TestSaveSessionReportsPersistentFailureOnce(t *testing.T) {
	a := newReloadApp(t, forcedFlags{})
	path := filepath.Join(t.TempDir(), "sessions.json")
	// A non-empty directory where the session file should be makes the final rename fail.
	if err := os.MkdirAll(filepath.Join(path, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	captureStderr(t, func() { a.sm = session.LoadSessionManager(path, a.modelRef) })

	out := captureStderr(t, func() {
		a.saveSession()
		a.saveSession()
		a.saveSession()
	})
	if n := strings.Count(out, "[session save failed:]"); n != 1 {
		t.Fatalf("failure reported %d times, want 1:\n%s", n, out)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after a failed save: %v", err)
	}

	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	out = captureStderr(t, a.saveSession)
	if !strings.Contains(out, "[session saved again]") {
		t.Errorf("recovery not announced: %q", out)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Errorf("session not written after recovery: %v", err)
	}
	if out := captureStderr(t, a.saveSession); out != "" {
		t.Errorf("a normal save printed %q", out)
	}
}

// A world-readable config holding a literal key, or a key sent over plain HTTP to another
// host, is warned about at startup and rejected with --strict, also on reload.
func TestConfigSecurityWarningsAndStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HMJ_CONFIG", path)
	write := func(baseURL string, mode os.FileMode) {
		t.Helper()
		cfg := `{"providers":{"remote":{"baseUrl":"` + baseURL + `","apiKey":"literal-key-123","models":[{"id":"m"}]}}}`
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	write("http://models.example.com/v1", 0o644)
	_, err := loadApp(forcedFlags{strict: true})
	if !errors.Is(err, errInsecureConfig) || !strings.Contains(err.Error(), "chmod 600") || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("--strict: err = %v, want both warnings as an error", err)
	}

	var a *app
	out := captureStderr(t, func() { a, err = loadApp(forcedFlags{}) })
	if err != nil || !strings.Contains(out, "chmod 600") || !strings.Contains(out, "unencrypted") {
		t.Fatalf("without --strict: err = %v, stderr = %q, want both warnings", err, out)
	}
	a.cancel()

	write("https://models.example.com/v1", 0o600)
	out = captureStderr(t, func() { a, err = loadApp(forcedFlags{strict: true}) })
	if err != nil || out != "" {
		t.Fatalf("secure config: err = %v, stderr = %q", err, out)
	}
	defer a.cancel()
	a.stampConfigLoaded()

	write("http://other.example.com/v1", 0o600)
	bumpMTime(t, path)
	out = captureStderr(t, a.reloadConfig)
	if !strings.Contains(out, "config reload ignored") || a.cfg.Providers["remote"].BaseURL != "https://models.example.com/v1" {
		t.Errorf("--strict reload of an insecure config: stderr = %q, baseUrl = %s", out, a.cfg.Providers["remote"].BaseURL)
	}
}

// The audit log and backups live next to the config; HMJ_AUDIT_LOG=off turns the log off.
func TestOpenAuditTrail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMJ_CONFIG", filepath.Join(dir, "config.json"))
	t.Cleanup(func() { tools.SetBackupDir("") })

	t.Setenv("HMJ_AUDIT_LOG", "off")
	openAuditTrail(io.Discard)()
	if _, err := os.Stat(filepath.Join(dir, "audit.log")); err == nil {
		t.Error("audit log created although HMJ_AUDIT_LOG=off")
	}

	t.Setenv("HMJ_AUDIT_LOG", "")
	closeAudit := openAuditTrail(io.Discard)
	defer closeAudit()
	if data, err := os.ReadFile(filepath.Join(dir, "audit.log")); err != nil || !strings.Contains(string(data), `"event":"start"`) {
		t.Errorf("audit log not started: %v, %q", err, data)
	}
}

// Errors name what failed, so they make sense without the preceding output.
func TestErrorsCarryContext(t *testing.T) {
	theme.SetAccessibility(true)
	t.Cleanup(func() { theme.SetAccessibility(false) })
	out := captureStderr(t, func() { reportError("/resume", errors.New("menu unavailable: not a terminal")) })
	if !strings.Contains(out, "[ERROR] /resume failed: menu unavailable: not a terminal") {
		t.Errorf("stderr = %q", out)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HMJ_CONFIG", path)
	for name, content := range map[string]string{
		"malformed": `{"providers":`,
		"invalid":   `{"providers":{"p":{"models":[{"id":"m"}]}}}`,
		"no models": `{"providers":{"p":{"baseUrl":"https://x","models":[]}}}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadApp(forcedFlags{}); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s config: err = %v, want it to name %s", name, err, path)
		}
	}
}

// TestWaitForTurnRefusesWhileTurnRuns checks that shutdown does not take the busy slot, and so does
// not save the session, while a turn still holds it past the grace period.
func TestWaitForTurnRefusesWhileTurnRuns(t *testing.T) {
	a := &app{busy: make(chan struct{}, 1)}
	a.busy <- struct{}{} // a running turn holds the slot

	if a.waitForTurn(10 * time.Millisecond) {
		t.Fatal("waitForTurn must report false while the turn holds the slot")
	}

	<-a.busy // the turn finishes
	if !a.waitForTurn(10 * time.Millisecond) {
		t.Fatal("waitForTurn must acquire the free slot")
	}
}
