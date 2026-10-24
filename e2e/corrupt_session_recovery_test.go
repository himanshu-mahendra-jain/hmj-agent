package e2e

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"testing"
)

// TestCorruptSessionFileIsSetAside starts hmj with a damaged conversation file: it says so,
// keeps the damaged file as a .corrupt backup, starts a fresh conversation, and saves it.
func TestCorruptSessionFileIsSetAside(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config("model-a"))
	damaged := conversationFile(e.Sessions, "1")
	if err := os.MkdirAll(filepath.Dir(damaged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damaged, []byte(`{"messages": [`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := e.Start()
	s.Expect("Conversation 1 could not be read and was set aside.")
	s.Expect("The damaged file was kept as " + damaged + ".corrupt.")
	s.ExpectPrompt()
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()

	backups, _ := filepath.Glob(damaged + ".corrupt.*")
	if len(backups) != 1 {
		t.Fatalf("corrupt backups = %q, want one", backups)
	}
	if got, _ := os.ReadFile(backups[0]); string(got) != `{"messages": [` {
		t.Errorf("backup content = %q, want the damaged file unchanged", got)
	}
	data, err := os.ReadFile(conversationFile(e.Sessions, "2"))
	if err != nil {
		t.Fatalf("no conversation saved after the recovery: %v", err)
	}
	if !jsontext.Value(data).IsValid() {
		t.Errorf("saved conversation is not valid JSON: %s", data)
	}
}
