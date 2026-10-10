package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCorruptSessionFileIsSetAside starts hmj with a damaged session file: it says so, keeps
// the damaged file as a .corrupt backup, starts fresh, and saves a valid session file again.
func TestCorruptSessionFileIsSetAside(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config("model-a"))
	if err := os.WriteFile(e.Sessions, []byte(`{"conversations": [`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := e.Start()
	s.Expect("Session history could not be read and has been reset.")
	s.Expect("The damaged file was kept as " + e.Sessions + ".corrupt.")
	s.ExpectPrompt()
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()

	backups, _ := filepath.Glob(e.Sessions + ".corrupt.*")
	if len(backups) != 1 {
		t.Fatalf("corrupt backups = %q, want one", backups)
	}
	if got, _ := os.ReadFile(backups[0]); string(got) != `{"conversations": [` {
		t.Errorf("backup content = %q, want the damaged file unchanged", got)
	}
	data, err := os.ReadFile(e.Sessions)
	if err != nil {
		t.Fatalf("no session file saved after the recovery: %v", err)
	}
	if !json.Valid(data) {
		t.Errorf("saved session file is not valid JSON: %s", data)
	}
}
