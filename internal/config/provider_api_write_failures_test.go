package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetProviderAPIRefusesBadRewrites checks the cases where writing "api" into the config would
// not give a valid config, or the file cannot be read as JSON at all: each is an error and the file
// is left exactly as it was.
func TestSetProviderAPIRefusesBadRewrites(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty provider object fails validation", `{"providers":{"p":{}}}`, "rewritten config is invalid"},
		{"wrong field type does not parse", `{"providers":{"p":{"baseUrl":"https://p.example","models":"m"}}}`, "rewritten config does not parse"},
		{"duplicate member", `{"providers":{"p":{"baseUrl":"https://a","baseUrl":"https://b"}}}`, "duplicate"},
		{"broken JSON", `{"providers":{"p":{"baseUrl":"https://a",}}}`, "invalid character"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.in, 0o600)
			err := SetProviderAPI(path, "p", APIResponses)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			// Regression: a config that failed to parse for its own reasons used to be reported
			// as one whose provider already sets "api".
			if errors.Is(err, ErrProviderAPISet) {
				t.Errorf("err = %v, wrongly blames an existing api member", err)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.in {
				t.Errorf("file changed to %s", got)
			}
		})
	}
}

// TestSetProviderAPIUnwritableDirectory checks that a config whose directory does not allow the
// temporary replacement file is reported as an error and left as it was.
func TestSetProviderAPIUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	in := `{"providers":{"p":{"baseUrl":"https://p.example","models":[{"id":"m"}]}}}`
	path := writeConfigFile(t, in, 0o600)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := SetProviderAPI(path, "p", APIAnthropic); err == nil {
		t.Fatal("expected an error when the config directory is not writable")
	}
	if got, _ := os.ReadFile(path); string(got) != in {
		t.Errorf("file changed to %s", got)
	}
}

// TestCreateStarterConfigRefusesFileItCannotMakePrivate checks that an existing empty file whose
// mode cannot be changed to 0600 is not filled with the starter config (which holds an API key
// placeholder the user fills in). /dev/null is such a file for anyone but root.
func TestCreateStarterConfigRefusesFileItCannotMakePrivate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may change the mode of /dev/null")
	}
	if _, err := os.Stat(os.DevNull); err != nil || os.DevNull != "/dev/null" {
		t.Skip("no /dev/null")
	}
	if err := CreateStarterConfig(os.DevNull, []byte(`{"providers":{}}`)); err == nil {
		t.Error("expected an error for a file whose mode cannot be changed")
	}
}

// TestCreateStarterConfigOnDirectory checks that a config path naming a directory is an error, not
// a starter config written somewhere else.
func TestCreateStarterConfigOnDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := CreateStarterConfig(dir, []byte("{}")); err == nil {
		t.Error("expected an error when the config path is a directory")
	}
}
