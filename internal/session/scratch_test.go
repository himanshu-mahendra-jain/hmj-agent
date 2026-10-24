package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/config"
)

// TestScratchDirsFollowTheirConversations checks that each saved conversation's scratch directory
// sits next to the session file under its ID, that deleting a conversation deletes it, and that an
// unsaved conversation has none.
func TestScratchDirsFollowTheirConversations(t *testing.T) {
	dir := t.TempDir()
	sm := LoadSessionManager(dir, config.ModelRef{Provider: "p", Model: "m"})
	first := sm.Active()
	second := sm.New("")
	if want := filepath.Join(dir, "scratch", first.ID); first.ScratchDir != want {
		t.Errorf("first: %q, want %q", first.ScratchDir, want)
	}
	if want := filepath.Join(dir, "scratch", second.ID); second.ScratchDir != want {
		t.Errorf("second: %q, want %q", second.ScratchDir, want)
	}
	for _, c := range []*Conversation{first, second} {
		if err := os.MkdirAll(c.ScratchDir, 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(c.ScratchDir, "run.log"), []byte("x"), 0o600)
	}
	if err := sm.Delete(second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.ScratchDir); !os.IsNotExist(err) {
		t.Errorf("the deleted conversation's scratch directory is still there: %v", err)
	}
	if _, err := os.Stat(first.ScratchDir); err != nil {
		t.Errorf("the other conversation's scratch directory is gone: %v", err)
	}

	if c := LoadSessionManager("", config.ModelRef{Provider: "p", Model: "m"}).Active(); c.ScratchDir != "" {
		t.Errorf("unsaved conversation has scratch directory %q", c.ScratchDir)
	}
}

// TestPruneScratchOnLoad checks which scratch directories loading the session directory removes:
// those not changed for 7 days, unless another hmj has the conversation open, and those of
// missing conversations after an hour.
func TestPruneScratchOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := dir
	sm := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	fresh, stale := sm.Active(), sm.New("")
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	scratchDir := func(name string, age time.Duration) string {
		d := filepath.Join(dir, "scratch", name)
		os.MkdirAll(filepath.Join(d, "sub"), 0o700)
		for _, p := range []string{filepath.Join(d, "sub", "a.log"), filepath.Join(d, "b.txt")} {
			os.WriteFile(p, []byte("x"), 0o600)
			os.Chtimes(p, now.Add(-age), now.Add(-age))
		}
		os.Chtimes(filepath.Join(d, "sub"), now.Add(-age), now.Add(-age))
		os.Chtimes(d, now.Add(-age), now.Add(-age))
		return d
	}
	keep := []string{scratchDir(fresh.ID, 6*24*time.Hour), scratchDir("99", 10*time.Minute)}
	staleDir, orphan := scratchDir(stale.ID, 8*24*time.Hour), scratchDir("98", 2*time.Hour)
	check := func(kept, gone []string) {
		t.Helper()
		for _, d := range kept {
			if _, err := os.Stat(d); err != nil {
				t.Errorf("%s was removed: %v", d, err)
			}
		}
		for _, d := range gone {
			if _, err := os.Stat(d); !os.IsNotExist(err) {
				t.Errorf("%s was kept", d)
			}
		}
	}

	// sm still has the stale conversation open, so its directory stays.
	other := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	check(append(keep, staleDir), []string{orphan})

	closeManager(t, sm)
	closeManager(t, other)
	LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	check(keep, []string{staleDir})
}

// TestScratchPromptNamesTheDirectory checks the system prompt's paragraph.
func TestScratchPromptNamesTheDirectory(t *testing.T) {
	p := ScratchPrompt("/home/u/.hmj/projects/x/scratch/3")
	for _, want := range []string{"/home/u/.hmj/projects/x/scratch/3", "$HMJ_SCRATCH", "not in /tmp or the project"} {
		if !strings.Contains(p, want) {
			t.Errorf("%q lacks %q", p, want)
		}
	}
}

// A directory whose age cannot be read is not taken for an idle one.
func TestLastChangeOfAMissingDirectoryIsAnError(t *testing.T) {
	if _, err := lastChange(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Error("lastChange of a missing directory returned no error")
	}
}
