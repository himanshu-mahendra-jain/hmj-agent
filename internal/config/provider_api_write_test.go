package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeConfigFile writes content to a temp config file with the given mode and returns its path.
func writeConfigFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetProviderAPI(t *testing.T) {
	t.Run("inserts on its own line with the object's indentation", func(t *testing.T) {
		in := "{\n  \"providers\": {\n    \"a\": {\n      \"baseUrl\": \"https://a.example/v1\",\n      \"apiKey\": \"k\",\n      \"models\": [{ \"id\": \"m\" }]\n    },\n    \"b\": {\n      \"baseUrl\": \"https://b.example/v1\",\n      \"models\": [{ \"id\": \"m\" }]\n    }\n  }\n}\n"
		path := writeConfigFile(t, in, 0o600)
		if err := SetProviderAPI(path, "b", "anthropic"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		want := "{\n  \"providers\": {\n    \"a\": {\n      \"baseUrl\": \"https://a.example/v1\",\n      \"apiKey\": \"k\",\n      \"models\": [{ \"id\": \"m\" }]\n    },\n    \"b\": {\n      \"api\": \"anthropic\",\n      \"baseUrl\": \"https://b.example/v1\",\n      \"models\": [{ \"id\": \"m\" }]\n    }\n  }\n}\n"
		if string(got) != want {
			t.Errorf("file after SetProviderAPI:\n%s\nwant:\n%s", got, want)
		}
		cfg, err := LoadConfig(path)
		if err != nil || cfg.Providers["b"].API != "anthropic" || cfg.Providers["a"].API != "" {
			t.Errorf("reloaded config = %+v, %v", cfg.Providers, err)
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 600", fi.Mode().Perm())
		}
	})

	t.Run("one-line object and tabs", func(t *testing.T) {
		in := "{\n\t\"providers\": {\n\t\t\"p\": { \"baseUrl\": \"https://p.example/v1\", \"models\": [{ \"id\": \"m\" }] }\n\t}\n}\n"
		path := writeConfigFile(t, in, 0o644)
		if err := SetProviderAPI(path, "p", "completions"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		want := "{\n\t\"providers\": {\n\t\t\"p\": { \"api\": \"completions\", \"baseUrl\": \"https://p.example/v1\", \"models\": [{ \"id\": \"m\" }] }\n\t}\n}\n"
		if string(got) != want {
			t.Errorf("file after SetProviderAPI:\n%s\nwant:\n%s", got, want)
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
			t.Errorf("mode = %o, want the original 644", fi.Mode().Perm())
		}
	})

	t.Run("provider names needing pointer escapes", func(t *testing.T) {
		in := "{\n  \"providers\": {\n    \"my/proxy~1\": {\n      \"baseUrl\": \"https://p.example/v1\",\n      \"models\": [{ \"id\": \"m\" }]\n    }\n  }\n}\n"
		path := writeConfigFile(t, in, 0o600)
		if err := SetProviderAPI(path, "my/proxy~1", "responses"); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil || cfg.Providers["my/proxy~1"].API != "responses" {
			t.Errorf("reloaded config = %+v, %v", cfg.Providers, err)
		}
	})

	t.Run("unknown provider leaves the file alone", func(t *testing.T) {
		in := "{\n  \"providers\": {\n    \"p\": {\n      \"baseUrl\": \"https://p.example/v1\",\n      \"models\": [{ \"id\": \"m\" }]\n    }\n  }\n}\n"
		path := writeConfigFile(t, in, 0o600)
		if err := SetProviderAPI(path, "other", "responses"); err == nil {
			t.Fatal("want an error for an unknown provider")
		}
		if got, _ := os.ReadFile(path); string(got) != in {
			t.Error("file changed")
		}
	})

	t.Run("provider that already sets api is not touched", func(t *testing.T) {
		in := "{\n  \"providers\": {\n    \"p\": {\n      \"baseUrl\": \"https://p.example/v1\",\n      \"api\": \"\",\n      \"models\": [{ \"id\": \"m\" }]\n    }\n  }\n}\n"
		path := writeConfigFile(t, in, 0o600)
		if err := SetProviderAPI(path, "p", "responses"); !errors.Is(err, ErrProviderAPISet) {
			t.Fatalf("err = %v, want ErrProviderAPISet", err)
		}
		if got, _ := os.ReadFile(path); string(got) != in {
			t.Error("file changed")
		}
	})

	t.Run("an unknown protocol is refused", func(t *testing.T) {
		in := "{\n  \"providers\": {\n    \"p\": {\n      \"baseUrl\": \"https://p.example/v1\",\n      \"models\": [{ \"id\": \"m\" }]\n    }\n  }\n}\n"
		path := writeConfigFile(t, in, 0o600)
		if err := SetProviderAPI(path, "p", "telepathy"); err == nil {
			t.Fatal("want the rewritten config to fail validation")
		}
		if got, _ := os.ReadFile(path); string(got) != in {
			t.Error("file changed")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if err := SetProviderAPI(filepath.Join(t.TempDir(), "none.json"), "p", "responses"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("err = %v, want not-exist", err)
		}
	})
}
