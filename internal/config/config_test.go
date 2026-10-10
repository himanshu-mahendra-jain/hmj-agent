package config

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestLoadConfigMinimalModelParameters(t *testing.T) {
	rawJSON := `{
  "providers": {
    "opencode": {
      "baseUrl": "https://opencode.ai/zen/v1",
      "apiKey": "test-key",
      "models": [
        {
          "id": "muse-spark-1.3-contributor-free",
          "name": "Muse Spark 1.3 Free"
        }
      ]
    }
  }
}`

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte(rawJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	p, ok := cfg.Providers["opencode"]
	if !ok {
		t.Fatalf("expected provider opencode to exist")
	}
	if p.BaseURL != "https://opencode.ai/zen/v1" {
		t.Errorf("expected BaseURL to be https://opencode.ai/zen/v1, got %s", p.BaseURL)
	}
	if len(p.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(p.Models))
	}

	m := p.Models[0]
	if m.ID != "muse-spark-1.3-contributor-free" {
		t.Errorf("expected model id muse-spark-1.3-contributor-free, got %s", m.ID)
	}
	if m.Name != "Muse Spark 1.3 Free" {
		t.Errorf("expected model name 'Muse Spark 1.3 Free', got %s", m.Name)
	}
	if m.DisplayName() != "Muse Spark 1.3 Free" {
		t.Errorf("expected DisplayName 'Muse Spark 1.3 Free', got %s", m.DisplayName())
	}

	// Test FindModel by Name, ID, or partial query
	ref, err := cfg.FindModel("muse-spark-1.3-contributor-free")
	if err != nil || ref.Model != "muse-spark-1.3-contributor-free" {
		t.Errorf("failed FindModel by ID: %v", err)
	}
	ref, err = cfg.FindModel("Muse Spark 1.3 Free")
	if err != nil || ref.Model != "muse-spark-1.3-contributor-free" {
		t.Errorf("failed FindModel by Name: %v", err)
	}
	ref, err = cfg.FindModel("muse spark")
	if err != nil || ref.Model != "muse-spark-1.3-contributor-free" {
		t.Errorf("failed FindModel by partial name: %v", err)
	}
}

func TestConfigWithoutAPIFieldLoads(t *testing.T) {
	rawJSON := `{
  "providers": {
    "local": {
      "baseUrl": "http://localhost:8080/v1",
      "models": [
        {"id": "test-model"}
      ]
    }
  }
}`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte(rawJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	if _, ok := cfg.Providers["local"]; !ok {
		t.Fatalf("expected provider local to exist")
	}
}

func TestConfigResolveModelAndErrors(t *testing.T) {
	cfg := Config{
		Providers: map[string]Provider{
			"p1": {
				BaseURL: "http://example.com",
				Models:  []Model{{ID: "m1", Name: "Model One"}},
			},
		},
	}

	// Successful resolution
	p, m, err := cfg.ResolveModel(ModelRef{Provider: "p1", Model: "m1"})
	if err != nil {
		t.Fatalf("unexpected error resolving model: %v", err)
	}
	if p.BaseURL != "http://example.com" || m.ID != "m1" {
		t.Errorf("unexpected resolution: p=%+v, m=%+v", p, m)
	}

	// Missing provider
	_, _, err = cfg.ResolveModel(ModelRef{Provider: "nonexistent", Model: "m1"})
	if err == nil {
		t.Error("expected error for nonexistent provider, got nil")
	}

	// Missing model in existing provider
	_, _, err = cfg.ResolveModel(ModelRef{Provider: "p1", Model: "nonexistent"})
	if err == nil {
		t.Error("expected error for nonexistent model, got nil")
	}
}

func TestWireAPIGeminiSelection(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		expected string
	}{
		{
			name:     "native gemini url autodetects gemini",
			provider: Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta"},
			expected: APIGemini,
		},
		{
			name:     "native gemini url with trailing slash",
			provider: Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta/"},
			expected: APIGemini,
		},
		{
			name:     "openai compat url uses completions",
			provider: Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"},
			expected: APICompletions,
		},
		{
			name:     "openai compat url with trailing slash uses completions",
			provider: Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/"},
			expected: APICompletions,
		},
		{
			name:     "explicit gemini",
			provider: Provider{BaseURL: "https://proxy.example.com/v1beta", API: "gemini"},
			expected: APIGemini,
		},
		{
			name:     "explicit gemini is case and space insensitive",
			provider: Provider{BaseURL: "https://proxy.example.com/v1beta", API: " Gemini "},
			expected: APIGemini,
		},
		{
			name:     "explicit api wins over gemini host",
			provider: Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta", API: "completions"},
			expected: APICompletions,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.provider.WireAPI(); got != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestLoadConfigAcceptsGemini(t *testing.T) {
	rawJSON := `{
  "providers": {
    "gemini": {
      "baseUrl": "https://generativelanguage.googleapis.com/v1beta",
      "apiKey": "$GEMINI_API_KEY",
      "api": "gemini",
      "models": [{ "id": "gemini-3.5-flash" }]
    }
  }
}`
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(rawJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("expected gemini api to load, got %v", err)
	}
	if got := cfg.Providers["gemini"].WireAPI(); got != APIGemini {
		t.Fatalf("expected gemini wire api, got %q", got)
	}
}

func TestLoadConfigUnknownAPIListsGemini(t *testing.T) {
	rawJSON := `{"providers":{"bad":{"baseUrl":"http://x","api":"vertex","models":[{"id":"m"}]}}}`
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(rawJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("expected error for unknown api")
	}
	if !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("expected error to list gemini as a valid option, got %v", err)
	}
}

func TestLoadConfigMissingFileWrapsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected errors.Is(err, os.ErrNotExist), got %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("expected error to name the path, got %v", err)
	}
}

func TestLoadConfigEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); !errors.Is(err, ErrEmptyConfig) {
		t.Fatalf("LoadConfig(empty) error = %v, want ErrEmptyConfig", err)
	}
}

// TestCreateStarterConfig checks that the starter config fills a missing or empty file with
// owner-only permissions and never replaces a file that has content.
func TestCreateStarterConfig(t *testing.T) {
	template := []byte(`{"providers":{}}`)
	dir := t.TempDir()

	t.Run("missing file and directory", func(t *testing.T) {
		path := filepath.Join(dir, "new", ".hmj", "config.json")
		if err := CreateStarterConfig(path, template); err != nil {
			t.Fatalf("CreateStarterConfig: %v", err)
		}
		if got, _ := os.ReadFile(path); string(got) != string(template) {
			t.Errorf("content = %q, want the template", got)
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
		}
	})
	t.Run("empty file is filled and tightened", func(t *testing.T) {
		path := filepath.Join(dir, "empty.json")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CreateStarterConfig(path, template); err != nil {
			t.Fatalf("CreateStarterConfig: %v", err)
		}
		if got, _ := os.ReadFile(path); string(got) != string(template) {
			t.Errorf("content = %q, want the template", got)
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
	})
	t.Run("file with content is left alone", func(t *testing.T) {
		path := filepath.Join(dir, "mine.json")
		if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CreateStarterConfig(path, template); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("CreateStarterConfig on existing file: error = %v, want fs.ErrExist", err)
		}
		if got, _ := os.ReadFile(path); string(got) != "keep me" {
			t.Errorf("existing file changed to %q", got)
		}
	})
}

func TestUnsetKeyVars(t *testing.T) {
	t.Setenv("HMJ_TEST_SET_KEY", "secret")
	t.Setenv("HMJ_TEST_EMPTY_KEY", "")
	cfg := Config{Providers: map[string]Provider{
		"set":     {APIKey: "$HMJ_TEST_SET_KEY"},
		"empty":   {APIKey: "$HMJ_TEST_EMPTY_KEY"},
		"literal": {APIKey: "sk-literal"},
		"none":    {},
	}}
	got := UnsetKeyVars(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "provider empty") || !strings.Contains(got[0], "$HMJ_TEST_EMPTY_KEY") {
		t.Errorf("UnsetKeyVars = %q, want one note for provider empty", got)
	}
}

func TestExampleConfigGeminiProvider(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("example config must load: %v", err)
	}
	p, ok := cfg.Providers["gemini"]
	if !ok {
		t.Fatal("expected gemini provider in config.example.json")
	}
	if p.WireAPI() != APIGemini {
		t.Fatalf("expected example gemini provider to use gemini wire api, got %q", p.WireAPI())
	}
}

func TestFormatVersion(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name:     "no vcs info",
			settings: []debug.BuildSetting{{Key: "-compiler", Value: "gc"}},
			want:     "1.0.0",
		},
		{
			name: "clean tree",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "1eea0d8c0ffee123456789abcdef0123456789ab"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "1.0.0 (1eea0d8)",
		},
		{
			name: "dirty tree",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "1eea0d8c0ffee123456789abcdef0123456789ab"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "1.0.0 (1eea0d8, dirty)",
		},
		{
			name: "short revision",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc"},
			},
			want: "1.0.0 (abc)",
		},
		{
			name: "revision without modified flag",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "1eea0d8c0ffee"},
				{Key: "vcs.time", Value: "2026-10-09T00:00:00Z"},
			},
			want: "1.0.0 (1eea0d8)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatVersion("1.0.0", tt.settings); got != tt.want {
				t.Fatalf("formatVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionStringStartsWithVersion(t *testing.T) {
	if got := VersionString(); !strings.HasPrefix(got, Version) {
		t.Fatalf("VersionString() = %q, want prefix %q", got, Version)
	}
}

func TestFindModelStrategies(t *testing.T) {
	cfg := Config{Providers: map[string]Provider{
		"anthropic": {Models: []Model{{ID: "opus", Name: "Claude Opus"}, {ID: "shared"}}},
		"gemini":    {Models: []Model{{ID: "gemini-flash", Name: "Flash"}, {ID: "shared"}}},
		"openai":    {Models: []Model{{ID: "gpt-luna", Name: "Luna"}}},
	}}
	// AllModels order (providers sorted): anthropic/opus, anthropic/shared,
	// gemini/gemini-flash, gemini/shared, openai/gpt-luna.
	cases := []struct {
		query   string
		want    string // provider/model, empty when an error is expected
		wantErr string
	}{
		{query: "1", want: "anthropic/opus"},
		{query: " 5 ", want: "openai/gpt-luna"},
		{query: "6", wantErr: `no model matching "6" found`},
		{query: "gemini/shared", want: "gemini/shared"},
		{query: "OpenAI/GPT-Luna", want: "openai/gpt-luna"},
		{query: "gemini-flash", want: "gemini/gemini-flash"},
		{query: "shared", wantErr: `ambiguous model "shared" matched: anthropic/shared, gemini/shared`},
		{query: "claude opus", want: "anthropic/opus"},
		{query: "flash", want: "gemini/gemini-flash"},
		{query: "lun", want: "openai/gpt-luna"},
		{query: "gemini", wantErr: `ambiguous query "gemini" matched: gemini/gemini-flash, gemini/shared`},
		{query: "nope", wantErr: `no model matching "nope" found`},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			ref, err := cfg.FindModel(tc.query)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("FindModel(%q) error = %v, want %q", tc.query, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindModel(%q) unexpected error: %v", tc.query, err)
			}
			if got := ref.Provider + "/" + ref.Model; got != tc.want {
				t.Fatalf("FindModel(%q) = %s, want %s", tc.query, got, tc.want)
			}
		})
	}

	if _, err := (Config{}).FindModel("x"); err == nil || err.Error() != "no models configured" {
		t.Fatalf("empty config error = %v, want %q", err, "no models configured")
	}
}

func TestLookupSentinelErrors(t *testing.T) {
	cfg := Config{Providers: map[string]Provider{"p": {Models: []Model{{ID: "m"}}}}}
	_, _, unknownProvider := cfg.ResolveModel(ModelRef{Provider: "x", Model: "m"})
	_, _, unknownModel := cfg.ResolveModel(ModelRef{Provider: "p", Model: "y"})
	_, noModels := Config{}.DefaultModel()
	cases := []struct {
		err      error
		sentinel error
		want     string
	}{
		{unknownProvider, ErrUnknownProvider, "unknown provider: x"},
		{unknownModel, ErrModelNotConfigured, "model not configured: p/y"},
		{noModels, ErrNoModels, "no models configured"},
	}
	for _, tc := range cases {
		if !errors.Is(tc.err, tc.sentinel) || tc.err.Error() != tc.want {
			t.Errorf("got %v, want %q matching %v", tc.err, tc.want, tc.sentinel)
		}
	}
}

// An API key never appears when a provider is printed, logged or encoded, but
// ResolveAPIKey still returns it.
func TestSecretIsMasked(t *testing.T) {
	const key = "sk-live-0123456789abcdef"
	p := Provider{BaseURL: "https://api.example.com", APIKey: key}

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("provider", "p", p, "key", p.APIKey)
	textLogged := new(bytes.Buffer)
	slog.New(slog.NewTextHandler(textLogged, nil)).Info("provider", "key", p.APIKey)
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{
		"%v": fmt.Sprintf("%v", p), "%+v": fmt.Sprintf("%+v", p), "%#v": fmt.Sprintf("%#v", p),
		"%s": fmt.Sprintf("%s", p.APIKey), "%q": fmt.Sprintf("%q", p.APIKey),
		"json": string(encoded), "slog json": logged.String(), "slog text": textLogged.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, key) {
			t.Errorf("%s leaked the key: %s", name, out)
		}
	}
	if got := ResolveAPIKey(p.APIKey); got != key {
		t.Errorf("ResolveAPIKey = %q, want the real key", got)
	}

	var decoded Provider
	if err := json.Unmarshal([]byte(`{"baseUrl":"x","apiKey":"`+key+`"}`), &decoded); err != nil || string(decoded.APIKey) != key {
		t.Errorf("decoding the config lost the key: %q, %v", decoded.APIKey, err)
	}
}

// "$NAME" is an environment lookup, never shell expansion: command substitution and
// nested variables are not evaluated.
func TestResolveAPIKeyDoesNotUseShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("HMJ_TEST_KEY", "real-key")
	tests := map[Secret]string{
		"$HMJ_TEST_KEY":                   "real-key",
		"$(touch " + Secret(marker) + ")": "",
		"$`touch " + Secret(marker) + "`": "",
		"${HMJ_TEST_KEY}":                 "",
		"literal-key":                     "literal-key",
	}
	for in, want := range tests {
		if got := ResolveAPIKey(in); got != want {
			t.Errorf("ResolveAPIKey(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a command in the key reference was executed")
	}
}

func TestSecurityWarnings(t *testing.T) {
	t.Setenv("HMJ_SET_KEY", "from-env")
	t.Setenv("HMJ_UNSET_KEY", "")
	tests := []struct {
		name    string
		baseURL string
		key     Secret
		mode    os.FileMode
		want    []string // substrings, one per expected warning
	}{
		{"private file, https", "https://api.example.com", "literal", 0o600, nil},
		{"readable file with literal key", "https://api.example.com", "literal", 0o644, []string{"mode 0644"}},
		{"readable file with env reference", "https://api.example.com", "$HMJ_SET_KEY", 0o644, nil},
		{"http to remote host", "http://api.example.com", "$HMJ_SET_KEY", 0o600, []string{"unencrypted to api.example.com"}},
		{"http without a key", "http://api.example.com", "$HMJ_UNSET_KEY", 0o600, nil},
		{"http to localhost", "http://localhost:11434/v1", "literal", 0o600, nil},
		{"http to 127.0.0.1", "http://127.0.0.1:8080", "literal", 0o600, nil},
		{"http to ::1", "http://[::1]:8080", "literal", 0o600, nil},
		{"both", "http://10.0.0.5:8000", "literal", 0o640, []string{"mode 0640", "unencrypted to 10.0.0.5:8000"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte("{}"), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Providers: map[string]Provider{"p": {BaseURL: tc.baseURL, APIKey: tc.key}}}
			got := SecurityWarnings(path, cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d warnings %q, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warning %d = %q, want it to mention %q", i, got[i], w)
				}
			}
		})
	}
}

// TestLoadConfigModelEffortAndOutputLimit checks that the per-model effort and output limit are read,
// that an unknown effort is rejected, and that a negative output limit is rejected.
func TestLoadConfigModelEffortAndOutputLimit(t *testing.T) {
	load := func(t *testing.T, models string) (Config, error) {
		t.Helper()
		raw := `{"providers":{"p":{"baseUrl":"http://x","api":"anthropic","models":[` + models + `]}}}`
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(path)
	}

	cfg, err := load(t, `{"id":"m","effort":"xhigh","maxOutputTokens":64000}`)
	if err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
	if m := cfg.Providers["p"].Models[0]; m.Effort != "xhigh" || m.MaxOutputTokens != 64000 {
		t.Errorf("model = %+v, want effort xhigh and maxOutputTokens 64000", m)
	}
	if _, err := load(t, `{"id":"m","effort":"extreme"}`); err == nil || !strings.Contains(err.Error(), "effort") {
		t.Errorf("unknown effort: err = %v, want an effort error", err)
	}
	if _, err := load(t, `{"id":"m","maxOutputTokens":-1}`); err == nil || !strings.Contains(err.Error(), "maxOutputTokens") {
		t.Errorf("negative maxOutputTokens: err = %v, want a maxOutputTokens error", err)
	}
}

// TestLoadConfigModelThinkingOption checks that thinking accepts "off" and "adaptive", that the default
// is on, and that any other value is rejected.
func TestLoadConfigModelThinkingOption(t *testing.T) {
	load := func(t *testing.T, models string) (Config, error) {
		t.Helper()
		raw := `{"providers":{"p":{"baseUrl":"http://x","api":"responses","models":[` + models + `]}}}`
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(path)
	}

	cfg, err := load(t, `{"id":"m","thinking":"off"},{"id":"n","thinking":"adaptive"},{"id":"o"}`)
	if err != nil {
		t.Fatalf("valid thinking values rejected: %v", err)
	}
	models := cfg.Providers["p"].Models
	if models[0].ThinkingEnabled() || !models[1].ThinkingEnabled() || !models[2].ThinkingEnabled() {
		t.Errorf("ThinkingEnabled = %v %v %v, want false true true", models[0].ThinkingEnabled(), models[1].ThinkingEnabled(), models[2].ThinkingEnabled())
	}
	if _, err := load(t, `{"id":"m","thinking":"maybe"}`); err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Errorf("unknown thinking value: err = %v, want a thinking error", err)
	}
}
