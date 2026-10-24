package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/config"
)

func TestConfigRejectsUnknownAPI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgContent := `{
		"providers": {
			"invalid": {
				"baseUrl": "https://api.openai.com/v1",
				"api": "unknown-protocol"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadConfig(cfgPath)
	if err == nil {
		t.Fatal("expected LoadConfig to reject unknown API protocol")
	}
	if !strings.Contains(err.Error(), "unknown api") {
		t.Errorf("expected 'unknown api' error, got: %v", err)
	}
}

func TestWorkspaceHashedSessionPath(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// Clear HMJ_SESSIONS so default hashing applies
	t.Setenv("HMJ_SESSIONS", "")

	t.Chdir(dir1)
	path1 := config.DefaultSessionDir()

	t.Chdir(dir2)
	path2 := config.DefaultSessionDir()

	if path1 == "" || path2 == "" {
		t.Fatalf("expected non-empty session paths, got %q and %q", path1, path2)
	}

	if !strings.Contains(path1, "projects") || !strings.Contains(path2, "projects") {
		t.Errorf("expected paths to contain 'projects' dir, got %q and %q", path1, path2)
	}

	if path1 == path2 {
		t.Errorf("expected different session paths for different directories, but got identical: %s", path1)
	}

	// Verify HMJ_SESSIONS override
	customPath := filepath.Join(dir1, "custom")
	t.Setenv("HMJ_SESSIONS", customPath)
	if got := config.DefaultSessionDir(); got != customPath {
		t.Errorf("expected HMJ_SESSIONS override to win, got %q", got)
	}
}

func TestWorktreeGitFileDetected(t *testing.T) {
	// Create mock git worktree structure:
	// worktreeDir/.git (a regular file, not a directory)
	// worktreeDir/subdir
	worktreeDir := t.TempDir()
	gitFilePath := filepath.Join(worktreeDir, ".git")
	if err := os.WriteFile(gitFilePath, []byte("gitdir: /path/to/main/repo/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subDir := filepath.Join(worktreeDir, "sub", "deep")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HMJ_SESSIONS", "")
	t.Chdir(subDir)

	sessPath := config.DefaultSessionDir()
	// Should identify worktreeDir as the project root and produce a valid path
	if !strings.Contains(sessPath, filepath.Base(worktreeDir)) {
		t.Errorf("expected worktree root %q to be detected from subdir, got %q", filepath.Base(worktreeDir), sessPath)
	}
}

func TestComprehensiveConfigResolution(t *testing.T) {
	// 1. ResolveAPIKey
	t.Setenv("TEST_AGENT_KEY", "secret-token-xyz")
	if got := config.ResolveAPIKey("$TEST_AGENT_KEY"); got != "secret-token-xyz" {
		t.Fatalf("expected 'secret-token-xyz', got %q", got)
	}
	if got := config.ResolveAPIKey("plain-literal-key"); got != "plain-literal-key" {
		t.Fatalf("expected 'plain-literal-key', got %q", got)
	}

	// 2. Model.DisplayName
	m1 := config.Model{ID: "m1", Name: "Model 1"}
	if m1.DisplayName() != "Model 1" {
		t.Fatalf("expected 'Model 1', got %q", m1.DisplayName())
	}
	m2 := config.Model{ID: "m2"}
	if m2.DisplayName() != "m2" {
		t.Fatalf("expected 'm2', got %q", m2.DisplayName())
	}

	// 3. FindModel ambiguous and errors
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"p1": {
				BaseURL: "http://example.com",
				Models: []config.Model{
					{ID: "gemini-pro", Name: "Gemini Pro"},
					{ID: "gemini-flash", Name: "Gemini Flash"},
				},
			},
			"p2": {
				BaseURL: "http://example.com",
				Models: []config.Model{
					{ID: "gemini-flash", Name: "Gemini Flash 2"},
				},
			},
		},
	}

	// Ambiguous ID match across providers
	_, err := cfg.FindModel("gemini-flash")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous error for gemini-flash, got %v", err)
	}

	// Empty models
	emptyCfg := config.Config{}
	_, err = emptyCfg.FindModel("any")
	if err == nil {
		t.Fatal("expected error finding model in empty config")
	}
	_, err = emptyCfg.DefaultModel()
	if err == nil {
		t.Fatal("expected error for DefaultModel with no models")
	}
}

func TestConfigAccessibilityField(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.json")

	cfgJSON := `{
		"accessibility": true,
		"providers": {
			"p1": {
				"baseUrl": "https://api.example.com",
				"models": [{"id": "m1"}]
			}
		}
	}`
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !cfg.Accessibility {
		t.Errorf("expected cfg.Accessibility to be true")
	}
}

func TestAllModelsSortedWithMapsKeys(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"zebra": {BaseURL: "https://z.com", Models: []config.Model{{ID: "z1"}}},
			"alpha": {BaseURL: "https://a.com", Models: []config.Model{{ID: "a1"}}},
			"beta":  {BaseURL: "https://b.com", Models: []config.Model{{ID: "b1"}}},
		},
	}

	models := cfg.AllModels()
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if models[0].Provider != "alpha" || models[1].Provider != "beta" || models[2].Provider != "zebra" {
		t.Fatalf("expected providers sorted alphabetically (alpha, beta, zebra), got: %v, %v, %v",
			models[0].Provider, models[1].Provider, models[2].Provider)
	}
}

func TestWireAPIAutoDetection(t *testing.T) {
	tests := []struct {
		provider config.Provider
		expected string
	}{
		{
			provider: config.Provider{BaseURL: "https://api.openai.com/v1", API: "completions"},
			expected: config.APICompletions,
		},
		{
			provider: config.Provider{BaseURL: "https://api.anthropic.com/v1", API: "anthropic"},
			expected: config.APIAnthropic,
		},
		{
			provider: config.Provider{BaseURL: "https://api.anthropic.com"},
			expected: config.APIAnthropic,
		},
		{
			provider: config.Provider{BaseURL: "https://custom.host/v1/messages"},
			expected: config.APIAnthropic,
		},
		{
			provider: config.Provider{BaseURL: "https://router.ai/v1/chat/completions"},
			expected: config.APICompletions,
		},
		{
			provider: config.Provider{BaseURL: "https://router.ai/v1/completions"},
			expected: config.APICompletions,
		},
		{
			provider: config.Provider{BaseURL: "https://api.openai.com/v1"},
			expected: config.APIResponses,
		},
	}

	for _, tt := range tests {
		got := tt.provider.WireAPI()
		if got != tt.expected {
			t.Errorf("provider %+v expected %q, got %q", tt.provider, tt.expected, got)
		}
	}
}

// TestConfigPathsAndResolution covers config accessors and model resolution.
func TestConfigPathsAndResolution(t *testing.T) {
	if config.DefaultConfigPath() == "" {
		t.Error("DefaultConfigPath returned empty string")
	}
	if config.DefaultSessionDir() == "" {
		t.Error("DefaultSessionDir returned empty string")
	}

	cfg := mockAgentConfig("http://127.0.0.1:1")
	p, err := cfg.Resolve(config.ModelRef{Provider: "mock", Model: "mock-model"})
	if err != nil || p.BaseURL != "http://127.0.0.1:1" {
		t.Errorf("Resolve failed: %v", err)
	}
	if _, err := cfg.Resolve(config.ModelRef{Provider: "nope"}); err == nil {
		t.Error("expected error resolving unknown provider")
	}

	ref, err := cfg.DefaultModel()
	if err != nil || ref.Provider != "mock" || ref.Model != "mock-model" {
		t.Errorf("DefaultModel failed: %+v %v", ref, err)
	}
}

func TestConfigHighContrastField(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	cfgJSON := `{
		"highContrast": true,
		"providers": {
			"p1": {
				"baseUrl": "https://api.example.com",
				"models": [{"id": "m1"}]
			}
		}
	}`
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !cfg.HighContrast {
		t.Errorf("expected cfg.HighContrast to be true")
	}
}
