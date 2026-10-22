package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/config"
)

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	os.Stderr = old
	w.Close()
	return <-out
}

// writeSessionFile writes content as a session file in a new directory and returns its path.
func writeSessionFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// skipAsRoot skips tests that rely on permission bits, which root ignores.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not apply to root")
	}
}

var testModel = config.ModelRef{Provider: "p", Model: "m"}

// TestLoadSessionManagerSurvivesNullConversations checks that a session file holding a null
// conversation (hand-edited, or written by an older version) loads the remaining
// conversations instead of crashing at startup.
func TestLoadSessionManagerSurvivesNullConversations(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		wantNames  []string
		wantActive string
	}{
		{
			name:       "null beside a real conversation",
			file:       `{"activeId":"1","order":["1","2"],"conversations":{"1":null,"2":{"id":"2","name":"two"}}}`,
			wantNames:  []string{"two"},
			wantActive: "two",
		},
		{
			name:       "only null conversations",
			file:       `{"activeId":"1","order":["1"],"conversations":{"1":null}}`,
			wantNames:  []string{"default"},
			wantActive: "default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := LoadSessionManager(writeSessionFile(t, tc.file), testModel)
			var names []string
			for _, c := range sm.List() {
				names = append(names, c.Name)
			}
			if strings.Join(names, ",") != strings.Join(tc.wantNames, ",") {
				t.Errorf("conversations = %v, want %v", names, tc.wantNames)
			}
			if got := sm.Active().Name; got != tc.wantActive {
				t.Errorf("active = %q, want %q", got, tc.wantActive)
			}
			if err := sm.Save(); err != nil {
				t.Errorf("Save after loading: %v", err)
			}
		})
	}
}

// TestLoadSessionManagerUsesTheKeyAsID checks that a conversation whose stored id is missing
// or differs from its key can still be resumed, renamed and deleted by the key that the
// order and the active ID use.
func TestLoadSessionManagerUsesTheKeyAsID(t *testing.T) {
	path := writeSessionFile(t, `{"activeId":"1","order":["1","2"],"conversations":{
		"1":{"id":"1","name":"one","model":{"provider":"p","model":"m"}},
		"2":{"name":"two","model":{"provider":"p","model":"m"}}}}`)
	sm := LoadSessionManager(path, testModel)
	conv, err := sm.Resume("two")
	if err != nil {
		t.Fatal(err)
	}
	if conv.ID != "2" || sm.Active().Name != "two" {
		t.Errorf("resumed id %q, active %q; want the conversation keyed 2 to be active", conv.ID, sm.Active().Name)
	}
	if err := sm.Delete(conv.ID); err != nil {
		t.Errorf("Delete(%q): %v", conv.ID, err)
	}
}

// TestLoadSessionManagerFinishesLoadedConversations checks what loading fills in: the ID counter
// continues after the highest ID, a conversation without a model gets the default, and an
// unstarted conversation gets the system prompt.
func TestLoadSessionManagerFinishesLoadedConversations(t *testing.T) {
	t.Chdir(t.TempDir())
	path := writeSessionFile(t, `{"activeId":"7","order":["7","3"],"counter":1,"conversations":{
		"7":{"id":"7","name":"started","model":{"provider":"p","model":"m"},"messages":[
			{"role":"system","content":"s"},{"role":"user","content":"run it"},{"role":"assistant","content":"done"}]},
		"3":{"id":"3","name":"unstarted","messages":[{"role":"assistant","content":"hello"}]}}}`)
	sm := LoadSessionManager(path, config.ModelRef{Provider: "fallback", Model: "f"})

	if sm.DefaultModel != testModel {
		t.Errorf("default model = %+v, want the active conversation's model", sm.DefaultModel)
	}
	unstarted := sm.Conversations["3"]
	if unstarted.Model.Model != "f" {
		t.Errorf("model of a conversation saved without one = %+v, want the default", unstarted.Model)
	}
	if unstarted.Messages[0].Role != "system" || unstarted.Messages[0].Content != CurrentSystemPrompt() || unstarted.Messages[1].Content != "hello" {
		t.Errorf("unstarted conversation messages = %+v, want the system prompt inserted first", unstarted.Messages)
	}
	if c := sm.New("next"); c.ID != "8" {
		t.Errorf("next ID = %s, want 8 (after the highest loaded ID)", c.ID)
	}
}

// TestLoadSessionManagerUnreadableFiles checks that a session file that cannot be read, or a
// corrupt one that cannot be moved aside, starts a fresh session with a warning.
func TestLoadSessionManagerUnreadableFiles(t *testing.T) {
	t.Run("path is a directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sessions.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		var sm *SessionManager
		out := captureStderr(t, func() { sm = LoadSessionManager(path, testModel) })
		if !strings.Contains(out, "warning: could not read session file") {
			t.Errorf("stderr = %q", out)
		}
		if len(sm.List()) != 1 || sm.Active().Name != "default" {
			t.Errorf("want one fresh default conversation, got %d", len(sm.List()))
		}
	})
	t.Run("corrupt file in a read-only directory", func(t *testing.T) {
		skipAsRoot(t)
		path := writeSessionFile(t, "{not json")
		dir := filepath.Dir(path)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		var sm *SessionManager
		out := captureStderr(t, func() { sm = LoadSessionManager(path, testModel) })
		if !strings.Contains(out, "could not be moved aside") || !strings.Contains(out, "Reason:") {
			t.Errorf("stderr = %q", out)
		}
		if sm.Active().Name != "default" {
			t.Errorf("active = %q, want a fresh default", sm.Active().Name)
		}
		if data, _ := os.ReadFile(path); string(data) != "{not json" {
			t.Errorf("the damaged file was changed: %q", data)
		}
	})
}

// TestSaveFailures checks that Save reports a session file it cannot write and leaves no
// temporary file behind.
func TestSaveFailures(t *testing.T) {
	t.Run("directory cannot be created", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		os.WriteFile(blocker, nil, 0o600)
		sm := NewSessionManager(testModel)
		sm.SetStoragePath(filepath.Join(blocker, "sessions.json"))
		if err := sm.Save(); err == nil {
			t.Error("Save succeeded under a regular file")
		}
	})
	t.Run("directory is read-only", func(t *testing.T) {
		skipAsRoot(t)
		dir := t.TempDir()
		os.Chmod(dir, 0o500)
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
		sm := NewSessionManager(testModel)
		sm.SetStoragePath(filepath.Join(dir, "sessions.json"))
		if err := sm.Save(); !errors.Is(err, os.ErrPermission) {
			t.Errorf("Save = %v, want a permission error", err)
		}
	})
	t.Run("no storage path", func(t *testing.T) {
		if err := NewSessionManager(testModel).Save(); err != nil {
			t.Errorf("Save without a path = %v, want nil", err)
		}
	})
}

// TestActiveFallsBack checks that Active repairs a dangling active ID: the first ordered
// conversation becomes active, or a default one is created when there are none.
func TestActiveFallsBack(t *testing.T) {
	sm := NewSessionManager(testModel)
	first := sm.ActiveID
	sm.New("second")
	sm.ActiveID = "ghost"
	if got := sm.Active(); got.ID != first || sm.ActiveID != first {
		t.Errorf("Active = %s, want the first ordered conversation %s", got.ID, first)
	}

	empty := &SessionManager{Conversations: map[string]*Conversation{}, DefaultModel: testModel}
	if got := empty.Active(); got == nil || got.Name != "default" || len(empty.List()) != 1 {
		t.Errorf("Active on an empty manager = %+v, want a new default conversation", got)
	}
}

// TestResumeQueries checks every way a conversation can be named for Resume, and the errors
// for queries that match none or several.
func TestResumeQueries(t *testing.T) {
	sm := NewSessionManager(testModel) // 1 "default"
	sm.New("deploy staging")           // 2
	sm.New("deploy prod")              // 3
	sm.New("Twin")                     // 4
	sm.New("twin")                     // 5
	cases := []struct {
		query   string
		wantID  string
		wantErr string
	}{
		{"3", "3", ""},           // ID
		{" 2 ", "2", ""},         // ID, trimmed
		{"DEPLOY PROD", "3", ""}, // name, any case
		{"staging", "2", ""},     // unique substring
		{"deploy", "", "ambiguous"},
		{"twin", "", "multiple conversations named"},
		{"nothing", "", "not found"},
		{"", "", "required"},
	}
	for _, tc := range cases {
		conv, err := sm.Resume(tc.query)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Resume(%q) err = %v, want %q", tc.query, err, tc.wantErr)
			}
			continue
		}
		if err != nil || conv.ID != tc.wantID || sm.ActiveID != tc.wantID {
			t.Errorf("Resume(%q) = %v, %v; want ID %s active", tc.query, conv, err, tc.wantID)
		}
	}
	if _, err := sm.Resume("nothing"); !errors.Is(err, ErrConversationNotFound) {
		t.Errorf("not-found error does not match ErrConversationNotFound: %v", err)
	}
}

// TestResumeByListPosition checks that a number that is not an ID picks the conversation at
// that position in the list.
func TestResumeByListPosition(t *testing.T) {
	sm := NewSessionManager(testModel)
	sm.New("b")
	sm.New("c")
	if err := sm.Delete("1"); err != nil {
		t.Fatal(err)
	}
	// IDs are now 2 and 3; "1" is the first position.
	conv, err := sm.Resume("1")
	if err != nil || conv.Name != "b" {
		t.Errorf("Resume(1) = %v, %v; want the first listed conversation", conv, err)
	}
}

// TestGenerateConversationTitleEdges checks titles for blank prompts and prompts that are
// only Markdown punctuation.
func TestGenerateConversationTitleEdges(t *testing.T) {
	cases := map[string]string{
		"   ":              "New conversation",
		"###":              "###",
		"## Fix the build": "Fix the build",
	}
	for prompt, want := range cases {
		if got := GenerateConversationTitle(prompt); got != want {
			t.Errorf("GenerateConversationTitle(%q) = %q, want %q", prompt, got, want)
		}
	}
}

// TestLoadSystemPromptSkipsUnusableGuidelines checks that an empty guideline file, or one
// with a line too long to read, is passed over for the next candidate.
func TestLoadSystemPromptSkipsUnusableGuidelines(t *testing.T) {
	cases := []struct {
		name   string
		agents string
	}{
		{"empty AGENTS.md", "  \n\n"},
		{"AGENTS.md with a line over 1MB", strings.Repeat("x", 2<<20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(tc.agents), 0o644)
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme rules"), 0o644)
			got := LoadSystemPrompt(dir)
			if !strings.Contains(got, "(README.md)") || !strings.Contains(got, "readme rules") || strings.Contains(got, "xxxx") {
				t.Errorf("prompt does not fall back to README.md: %q", got[len(DefaultSystemPrompt):])
			}
		})
	}
	t.Run("unreadable AGENTS.md", func(t *testing.T) {
		skipAsRoot(t)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("secret"), 0o000)
		os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme rules"), 0o644)
		if got := LoadSystemPrompt(dir); !strings.Contains(got, "readme rules") {
			t.Errorf("prompt does not fall back to README.md: %q", got[len(DefaultSystemPrompt):])
		}
	})
}

// TestCurrentSystemPromptInDeletedWorkingDirectory checks that a working directory removed
// from under hmj yields the default system prompt instead of an error or a stale guideline.
func TestCurrentSystemPromptInDeletedWorkingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("rules of a deleted project"), 0o644)
	t.Chdir(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got := CurrentSystemPrompt(); got != DefaultSystemPrompt {
		t.Errorf("prompt = %q, want the default", got[min(len(got), len(DefaultSystemPrompt)):])
	}
}
