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

// writeSessionDir writes a session directory in a new temporary directory and returns it:
// state.json, when state is not empty, and each conversation as conversations/<id>.json.
func writeSessionDir(t *testing.T, state string, conversations map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conversations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for id, conv := range conversations {
		if err := os.WriteFile(filepath.Join(dir, "conversations", id+".json"), []byte(conv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// skipAsRoot skips tests that rely on permission bits, which root ignores.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not apply to root")
	}
}

var testModel = config.ModelRef{Provider: "p", Model: "m"}

// TestLoadSessionManagerDropsIDsThatAreNotOnePathElement checks that conversation files whose
// name gives an ID of "", "." or "..", and the temporary files writes leave, are not loaded: such
// an ID's scratch directory would lie outside the scratch root, and deleting the conversation
// would remove it.
func TestLoadSessionManagerDropsIDsThatAreNotOnePathElement(t *testing.T) {
	dir := writeSessionDir(t, `{"activeId":".."}`, map[string]string{
		"": `{"name":"empty"}`, ".": `{"name":"dot"}`, "..": `{"name":"parent"}`,
		".ok.json.tmp1": `{"name":"temporary"}`, "ok": `{"name":"kept"}`,
	})
	outside := filepath.Join(dir, "keep-me")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	sm := LoadSessionManager(dir, testModel)
	list := sm.List()
	if len(list) != 1 || list[0].Name != "kept" || sm.Active().Name != "kept" {
		t.Fatalf("loaded %d conversations, active %q; want only the one named ok", len(list), sm.Active().Name)
	}
	if got := filepath.Dir(list[0].ScratchDir); got != filepath.Join(dir, "scratch") {
		t.Errorf("scratch directory %s is not in the scratch root", list[0].ScratchDir)
	}
	if err := sm.Delete("ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a directory beside the scratch root went with the conversation: %v", err)
	}
}

// TestLoadSessionManagerUsesTheKeyAsID checks that a conversation whose stored id is missing
// or differs from its file name can still be resumed, renamed and deleted by the file name,
// which the active ID uses.
func TestLoadSessionManagerUsesTheKeyAsID(t *testing.T) {
	dir := writeSessionDir(t, `{"activeId":"1"}`, map[string]string{
		"1": `{"id":"1","name":"one","model":{"provider":"p","model":"m"}}`,
		"2": `{"id":"9","name":"two","model":{"provider":"p","model":"m"}}`,
	})
	sm := LoadSessionManager(dir, testModel)
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
	dir := writeSessionDir(t, `{"activeId":"7"}`, map[string]string{
		"7": `{"id":"7","name":"started","model":{"provider":"p","model":"m"},"messages":[
			{"role":"system","content":"s"},{"role":"user","content":"run it"},{"role":"assistant","content":"done"}]}`,
		"3": `{"id":"3","name":"unstarted","messages":[{"role":"assistant","content":"hello"}]}`,
	})
	sm := LoadSessionManager(dir, config.ModelRef{Provider: "fallback", Model: "f"})

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

// TestLoadSessionManagerUnreadableFiles checks that a conversation file that cannot be read is
// passed over, and a corrupt one that cannot be moved aside is reported, while hmj starts with a
// fresh conversation.
func TestLoadSessionManagerUnreadableFiles(t *testing.T) {
	t.Run("conversation file is a directory", func(t *testing.T) {
		dir := writeSessionDir(t, "", nil)
		if err := os.Mkdir(filepath.Join(dir, "conversations", "1.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		sm := LoadSessionManager(dir, testModel)
		if len(sm.List()) != 1 || sm.Active().Name != "default" || sm.ActiveID != "2" {
			t.Errorf("want one fresh default conversation with ID 2, got %d, active %s", len(sm.List()), sm.ActiveID)
		}
	})
	t.Run("corrupt file in a read-only directory", func(t *testing.T) {
		skipAsRoot(t)
		dir := writeSessionDir(t, "", map[string]string{"1": "{not json"})
		convDir := filepath.Join(dir, "conversations")
		if err := os.Chmod(convDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(convDir, 0o700) })
		var sm *SessionManager
		out := captureStderr(t, func() { sm = LoadSessionManager(dir, testModel) })
		if !strings.Contains(out, "could not be moved aside") || !strings.Contains(out, "Reason:") {
			t.Errorf("stderr = %q", out)
		}
		if sm.Active().Name != "default" {
			t.Errorf("active = %q, want a fresh default", sm.Active().Name)
		}
		if data, _ := os.ReadFile(filepath.Join(convDir, "1.json")); string(data) != "{not json" {
			t.Errorf("the damaged file was changed: %q", data)
		}
	})
}

// TestSaveFailures checks that Save reports a conversation it cannot write and leaves no
// temporary file behind.
func TestSaveFailures(t *testing.T) {
	t.Run("directory cannot be created", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		sm := LoadSessionManager(filepath.Join(blocker, "sessions"), testModel)
		if err := sm.Save(); err == nil {
			t.Error("Save succeeded under a regular file")
		}
	})
	t.Run("directory is read-only", func(t *testing.T) {
		skipAsRoot(t)
		dir := t.TempDir()
		sm := LoadSessionManager(dir, testModel)
		convDir := filepath.Join(dir, "conversations")
		if err := os.Chmod(convDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(convDir, 0o700) })
		if err := sm.Save(); !errors.Is(err, os.ErrPermission) {
			t.Errorf("Save = %v, want a permission error", err)
		}
		if temps, _ := filepath.Glob(filepath.Join(convDir, ".*")); len(temps) > 0 {
			t.Errorf("temporary files left behind: %v", temps)
		}
	})
	t.Run("no storage path", func(t *testing.T) {
		if err := LoadSessionManager("", testModel).Save(); err != nil {
			t.Errorf("Save without a path = %v, want nil", err)
		}
	})
}

// TestActiveFallsBack checks that Active repairs a dangling active ID: the first ordered
// conversation becomes active, or a default one is created when there are none.
func TestActiveFallsBack(t *testing.T) {
	sm := LoadSessionManager("", testModel)
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
	sm := LoadSessionManager("", testModel) // 1 "default"
	sm.New("deploy staging")                // 2
	sm.New("deploy prod")                   // 3
	sm.New("Twin")                          // 4
	sm.New("twin")                          // 5
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
	sm := LoadSessionManager("", testModel)
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
