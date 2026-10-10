package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCovConfig writes body to a fresh config.json under t.TempDir and returns its path.
func writeCovConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigRejectsBadFiles(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string // substring of the error; "" means any error
		empty   bool   // error must wrap ErrEmptyConfig
	}{
		{name: "whitespace only is empty", body: " \n\t\n", empty: true},
		{name: "invalid JSON", body: `{"providers": `, wantErr: "config.json"},
		{name: "provider without baseUrl", body: `{"providers":{"p":{"apiKey":"x"}}}`, wantErr: "provider p has no baseUrl"},
		{name: "unknown api", body: `{"providers":{"p":{"baseUrl":"https://x","api":"bogus"}}}`, wantErr: `unknown api "bogus"`},
		{name: "unknown effort", body: `{"providers":{"p":{"baseUrl":"https://x","models":[{"id":"m","effort":"ultra"}]}}}`, wantErr: `unknown effort "ultra"`},
		{name: "unknown thinking", body: `{"providers":{"p":{"baseUrl":"https://x","models":[{"id":"m","thinking":"on"}]}}}`, wantErr: `unknown thinking "on"`},
		{name: "negative maxOutputTokens", body: `{"providers":{"p":{"baseUrl":"https://x","models":[{"id":"m","maxOutputTokens":-1}]}}}`, wantErr: "negative maxOutputTokens -1"},
		{name: "wrong JSON type for models", body: `{"providers":{"p":{"baseUrl":"https://x","models":"m"}}}`, wantErr: "config.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeCovConfig(t, tc.body))
			if err == nil {
				t.Fatal("LoadConfig succeeded, want an error")
			}
			if tc.empty && !errors.Is(err, ErrEmptyConfig) {
				t.Errorf("error = %v, want ErrEmptyConfig", err)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigAcceptsValidFieldValues(t *testing.T) {
	body := `{"providers":{"p":{"baseUrl":"https://x","api":"  ANTHROPIC ","models":[
		{"id":"a","effort":"max","thinking":"adaptive","maxOutputTokens":0},
		{"id":"b","effort":"","thinking":"","contextWindow":200000},
		{"id":"c","thinking":"off","maxOutputTokens":4096}
	]}}}`
	cfg, err := LoadConfig(writeCovConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	models := cfg.Providers["p"].Models
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3", len(models))
	}
	if models[2].ThinkingEnabled() {
		t.Error(`thinking "off" should disable reasoning parameters`)
	}
	if !models[0].ThinkingEnabled() || !models[1].ThinkingEnabled() {
		t.Error(`thinking "" and "adaptive" should keep reasoning enabled`)
	}
	if models[1].ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", models[1].ContextWindow)
	}
}

func TestLoadConfigMissingFileReturnsNotExistError(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil || !strings.Contains(err.Error(), "config file not found") {
		t.Fatalf("error = %v, want 'config file not found'", err)
	}
}

func TestReloadConfigKeepsCurrentOnFailure(t *testing.T) {
	current := Config{Providers: map[string]Provider{"keep": {BaseURL: "https://keep"}}}

	t.Run("missing file keeps current", func(t *testing.T) {
		got, err := ReloadConfig(filepath.Join(t.TempDir(), "gone.json"), current)
		if err == nil {
			t.Fatal("want error")
		}
		if _, ok := got.Providers["keep"]; !ok {
			t.Errorf("current config not returned: %+v", got)
		}
	})
	t.Run("no providers keeps current", func(t *testing.T) {
		path := writeCovConfig(t, `{"providers":{}}`)
		got, err := ReloadConfig(path, current)
		if err == nil || !strings.Contains(err.Error(), "no providers configured") {
			t.Fatalf("error = %v, want 'no providers configured'", err)
		}
		if _, ok := got.Providers["keep"]; !ok {
			t.Errorf("current config not returned: %+v", got)
		}
	})
	t.Run("valid file replaces current", func(t *testing.T) {
		path := writeCovConfig(t, `{"providers":{"new":{"baseUrl":"https://new"}}}`)
		got, err := ReloadConfig(path, current)
		if err != nil {
			t.Fatalf("ReloadConfig: %v", err)
		}
		if _, ok := got.Providers["new"]; !ok || len(got.Providers) != 1 {
			t.Errorf("providers = %+v, want only \"new\"", got.Providers)
		}
	})
}

func TestResolveAndDefaultModelEdges(t *testing.T) {
	var empty Config
	if _, err := empty.DefaultModel(); !errors.Is(err, ErrNoModels) {
		t.Errorf("DefaultModel on empty config: error = %v, want ErrNoModels", err)
	}
	if got := empty.AllModels(); len(got) != 0 {
		t.Errorf("AllModels on empty config = %v, want none", got)
	}

	cfg := Config{Providers: map[string]Provider{
		"b": {BaseURL: "https://b", Models: []Model{{ID: "m2"}}},
		"a": {BaseURL: "https://a", Models: []Model{{ID: "m1"}}},
	}}
	ref, err := cfg.DefaultModel()
	if err != nil || ref != (ModelRef{Provider: "a", Model: "m1"}) {
		t.Errorf("DefaultModel = %v, %v; want a/m1 (providers sorted by name)", ref, err)
	}

	if _, err := cfg.Resolve(ModelRef{Provider: "zzz", Model: "m1"}); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("Resolve unknown provider: error = %v", err)
	}
	if _, err := cfg.Resolve(ModelRef{Provider: "a", Model: "nope"}); !errors.Is(err, ErrModelNotConfigured) {
		t.Errorf("Resolve unknown model: error = %v", err)
	}
	p, err := cfg.Resolve(ModelRef{Provider: "b", Model: "m2"})
	if err != nil || p.BaseURL != "https://b" {
		t.Errorf("Resolve b/m2 = %+v, %v", p, err)
	}
}

func TestModelDisplayName(t *testing.T) {
	if got := (Model{ID: "gpt-x", Name: "GPT X"}).DisplayName(); got != "GPT X" {
		t.Errorf("DisplayName with name = %q", got)
	}
	if got := (Model{ID: "gpt-x"}).DisplayName(); got != "gpt-x" {
		t.Errorf("DisplayName without name = %q", got)
	}
}

func TestFindModelRejectsOutOfRangeIndexAndAmbiguousDisplayName(t *testing.T) {
	cfg := Config{Providers: map[string]Provider{
		"p": {BaseURL: "https://p", Models: []Model{{ID: "one", Name: "Shared"}}},
		"q": {BaseURL: "https://q", Models: []Model{{ID: "two", Name: "shared"}}},
	}}
	if _, err := cfg.FindModel("9"); err == nil {
		t.Error("index 9 out of range should not match")
	}
	if _, err := cfg.FindModel("0"); err == nil {
		t.Error("index 0 is not 1-based and should not match")
	}
	_, err := cfg.FindModel("SHARED")
	if err == nil || !strings.Contains(err.Error(), "ambiguous model name") {
		t.Errorf("FindModel(SHARED) error = %v, want ambiguous model name", err)
	}
	if _, err := (Config{}).FindModel("x"); !errors.Is(err, ErrNoModels) {
		t.Errorf("FindModel on empty config: error = %v, want ErrNoModels", err)
	}
}

func TestSecretMaskingEncodings(t *testing.T) {
	var empty Secret
	if empty.String() != "" || empty.GoString() != `""` {
		t.Errorf("empty secret should render empty, got %q / %q", empty.String(), empty.GoString())
	}
	s := Secret("sk-live")
	if got := s.String(); got != "[redacted]" {
		t.Errorf("String = %q", got)
	}
	if got := s.GoString(); got != `"[redacted]"` {
		t.Errorf("GoString = %q", got)
	}
	if got := s.LogValue().String(); got != "[redacted]" {
		t.Errorf("LogValue = %q", got)
	}
	b, err := s.MarshalJSON()
	if err != nil || string(b) != `"[redacted]"` {
		t.Errorf("MarshalJSON = %s, %v", b, err)
	}
}

func TestResolveAPIKeyVariants(t *testing.T) {
	t.Setenv("HMJ_COV_KEY", "secret-value")
	if got := ResolveAPIKey("$HMJ_COV_KEY"); got != "secret-value" {
		t.Errorf("ResolveAPIKey($VAR) = %q", got)
	}
	if got := ResolveAPIKey("literal-key"); got != "literal-key" {
		t.Errorf("ResolveAPIKey(literal) = %q", got)
	}
	if got := ResolveAPIKey("$HMJ_COV_UNSET_NOPE"); got != "" {
		t.Errorf("ResolveAPIKey(unset var) = %q, want empty", got)
	}
}

func TestUnsetKeyVarsReportsEmptyAndMissing(t *testing.T) {
	t.Setenv("HMJ_COV_EMPTY", "")
	t.Setenv("HMJ_COV_SET", "yes")
	cfg := Config{Providers: map[string]Provider{
		"a": {BaseURL: "https://a", APIKey: "$HMJ_COV_EMPTY"},
		"b": {BaseURL: "https://b", APIKey: "$HMJ_COV_SET"},
		"c": {BaseURL: "https://c", APIKey: "literal"},
		"d": {BaseURL: "https://d", APIKey: "$HMJ_COV_MISSING"},
	}}
	got := UnsetKeyVars(cfg)
	if len(got) != 2 {
		t.Fatalf("UnsetKeyVars = %v, want 2 notes (a and d)", got)
	}
	if !strings.Contains(got[0], "provider a") || !strings.Contains(got[1], "provider d") {
		t.Errorf("notes out of order or wrong: %v", got)
	}
}

func TestSanitizeProjectSlug(t *testing.T) {
	tests := map[string]string{
		"hmj-agent":   "hmj-agent",
		"my project":  "my_project",
		"--weird--":   "weird",
		"a.b/c":       "a_b_c",
		"___":         "project",
		"":            "project",
		"héllo":       "h_llo",
		"Mixed_Case9": "Mixed_Case9",
	}
	for in, want := range tests {
		if got := sanitizeProjectSlug(in); got != want {
			t.Errorf("sanitizeProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultConfigPathHonoursEnvAndHome(t *testing.T) {
	t.Setenv("HMJ_CONFIG", "/custom/path.json")
	if got := DefaultConfigPath(); got != "/custom/path.json" {
		t.Errorf("DefaultConfigPath with HMJ_CONFIG = %q", got)
	}

	t.Setenv("HMJ_CONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".hmj", "config.json")
	if got := DefaultConfigPath(); got != want {
		t.Errorf("DefaultConfigPath = %q, want %q", got, want)
	}
}

func TestDefaultSessionPathEnvOverrideAndProjectScope(t *testing.T) {
	t.Setenv("HMJ_SESSIONS", "/explicit/sessions.json")
	if got := DefaultSessionPath(); got != "/explicit/sessions.json" {
		t.Errorf("DefaultSessionPath with HMJ_SESSIONS = %q", got)
	}

	t.Setenv("HMJ_SESSIONS", "")
	t.Setenv("HMJ_CONFIG", filepath.Join(t.TempDir(), "cfg", "config.json"))
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(project, "pkg", "inner")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	got := DefaultSessionPath()
	base := filepath.Base(project)
	if filepath.Base(filepath.Dir(filepath.Dir(got))) != "projects" {
		t.Errorf("DefaultSessionPath = %q, want <config dir>/projects/<slug>/sessions.json", got)
	}
	if !strings.Contains(filepath.Base(filepath.Dir(got)), sanitizeProjectSlug(base)) {
		t.Errorf("slug directory in %q does not name project %q", got, base)
	}
	if filepath.Base(got) != "sessions.json" {
		t.Errorf("file name = %q, want sessions.json", filepath.Base(got))
	}
	if filepath.Dir(filepath.Dir(filepath.Dir(got))) != filepath.Dir(os.Getenv("HMJ_CONFIG")) {
		t.Errorf("session path %q is not under the config directory", got)
	}

	// Same workspace from the project root yields the same path.
	t.Chdir(project)
	if again := DefaultSessionPath(); again != got {
		t.Errorf("path changes between subdirectory and root: %q vs %q", got, again)
	}
}

func TestFindProjectRootFallsBackToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := findProjectRoot(); got != wd {
		t.Errorf("findProjectRoot without .git = %q, want cwd %q", got, wd)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := map[string]bool{
		"localhost":          true,
		"LOCALHOST":          true,
		"api.localhost":      true,
		"127.0.0.2":          true,
		"::1":                true,
		"example.com":        false,
		"10.0.0.1":           false,
		"localhost.evil.com": false,
		"":                   false,
	}
	for host, want := range tests {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestWireAPIInferenceFromURLShapes(t *testing.T) {
	tests := []struct {
		name    string
		api     string
		baseURL string
		want    string
	}{
		{"explicit mixed case with spaces", "  Responses ", "https://x", APIResponses},
		{"explicit completions beats anthropic host", "Completions", "https://api.anthropic.com", APICompletions},
		{"explicit anthropic beats gemini host", "anthropic", "https://generativelanguage.googleapis.com", APIAnthropic},
		{"explicit gemini", "GEMINI", "https://example.com/v1", APIGemini},
		{"gemini native host", "", "https://generativelanguage.googleapis.com/v1beta", APIGemini},
		{"gemini openai compat path", "", "https://generativelanguage.googleapis.com/v1beta/openai/", APICompletions},
		{"anthropic host", "", "https://api.anthropic.com", APIAnthropic},
		{"anthropic substring in proxy host", "", "https://anthropic-proxy.internal/v1", APIAnthropic},
		{"messages suffix with trailing slash", "", "https://proxy.example/v1/messages/", APIAnthropic},
		{"chat completions suffix", "", "https://proxy.example/v1/chat/completions", APICompletions},
		{"completions suffix", "", "https://proxy.example/v1/completions", APICompletions},
		{"plain host defaults to responses", "", "https://api.openai.com/v1", APIResponses},
		{"empty URL defaults to responses", "", "", APIResponses},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{API: tc.api, BaseURL: tc.baseURL}
			if got := p.WireAPI(); got != tc.want {
				t.Errorf("WireAPI(api=%q, base=%q) = %q, want %q", tc.api, tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestLoadConfigReadError(t *testing.T) {
	// A directory is readable as a path but not as file content; the error is returned, not swallowed.
	if _, err := LoadConfig(t.TempDir()); err == nil {
		t.Error("LoadConfig on a directory should fail")
	}
}

func TestCreateStarterConfigFailsWhenParentIsFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateStarterConfig(filepath.Join(blocker, "config.json"), []byte("{}")); err == nil {
		t.Error("CreateStarterConfig under a regular file should fail")
	}
}

func TestDefaultConfigPathWithoutHomeFallsBackToCwdName(t *testing.T) {
	t.Setenv("HMJ_CONFIG", "")
	t.Setenv("HOME", "")
	if got := DefaultConfigPath(); got != "config.json" {
		t.Errorf("DefaultConfigPath with no HOME = %q, want config.json", got)
	}
}
