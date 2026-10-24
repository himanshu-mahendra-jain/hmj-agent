package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
)

// say appends a user message to conv.
func say(conv *Conversation, text string) {
	conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: text})
}

// saved returns the user messages of conversation id as its file holds them.
func saved(t *testing.T, dir, id string) []string {
	t.Helper()
	sm := &SessionManager{dir: dir, Conversations: map[string]*Conversation{}, stamps: map[string]fileStamp{}}
	conv, _, err := sm.readConversation(id)
	if err != nil {
		t.Fatal(err)
	}
	return prompts(conv.Messages)
}

// TestTwoManagersShareASessionDirectory checks that two hmj processes in one project each open
// a conversation of their own, and that their saves keep each other's turns: each writes only the
// conversation it has open.
func TestTwoManagersShareASessionDirectory(t *testing.T) {
	dir := t.TempDir()
	a := LoadSessionManager(dir, testModel)
	say(a.Active(), "a1")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	b := LoadSessionManager(dir, testModel)
	if b.ActiveID == a.ActiveID {
		t.Fatalf("both opened conversation %s", a.ActiveID)
	}
	say(b.Active(), "b1")
	say(a.Active(), "a2")
	for _, sm := range []*SessionManager{b, a, b} {
		if err := sm.Save(); err != nil {
			t.Fatal(err)
		}
	}

	if got := saved(t, dir, a.ActiveID); !slices.Equal(got, []string{"a1", "a2"}) {
		t.Errorf("a's conversation holds %q, want a1 and a2", got)
	}
	if got := saved(t, dir, b.ActiveID); !slices.Equal(got, []string{"b1"}) {
		t.Errorf("b's conversation holds %q, want b1", got)
	}
}

// TestNewConversationsGetDistinctIDs checks that conversations two managers create in turn never
// share an ID.
func TestNewConversationsGetDistinctIDs(t *testing.T) {
	dir := t.TempDir()
	a, b := LoadSessionManager(dir, testModel), LoadSessionManager(dir, testModel)
	ids := []string{a.ActiveID, b.ActiveID}
	for i := range 6 {
		sm := a
		if i%2 == 1 {
			sm = b
		}
		ids = append(ids, sm.New("").ID)
	}
	slices.Sort(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		t.Errorf("IDs %v repeat", ids)
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(dir, "conversations", id+".json")); err != nil {
			t.Errorf("conversation %s has no file: %v", id, err)
		}
	}
}

// TestConversationOpenElsewhereIsRefused checks that a conversation another manager has open
// cannot be opened, renamed or deleted, is marked as open elsewhere, and can be opened, with the
// other manager's last turns, once that manager has moved on.
func TestConversationOpenElsewhereIsRefused(t *testing.T) {
	dir := t.TempDir()
	a := LoadSessionManager(dir, testModel)
	held := a.ActiveID
	b := LoadSessionManager(dir, testModel)
	own := b.ActiveID

	if !b.OpenElsewhere(held) || b.OpenElsewhere(own) || a.OpenElsewhere(held) {
		t.Errorf("OpenElsewhere: held %v, own %v, held by itself %v; want true, false, false",
			b.OpenElsewhere(held), b.OpenElsewhere(own), a.OpenElsewhere(held))
	}
	_, resumeErr := b.Resume(held)
	for name, err := range map[string]error{"resume": resumeErr, "rename": b.Rename(held, "x"), "delete": b.Delete(held)} {
		if !errors.Is(err, ErrOpenElsewhere) || !strings.Contains(err.Error(), fmt.Sprintf("(pid %d)", os.Getpid())) {
			t.Errorf("%s = %v, want ErrOpenElsewhere naming this process", name, err)
		}
	}
	if b.ActiveID != own || a.Conversations[held].Name == "x" {
		t.Errorf("a refused call changed state: b active %s, name %q", b.ActiveID, a.Conversations[held].Name)
	}

	say(a.Active(), "last word")
	a.New("") // a leaves the conversation, saving it
	conv, err := b.Resume(held)
	if err != nil {
		t.Fatal(err)
	}
	if got := prompts(conv.Messages); !slices.Equal(got, []string{"last word"}) {
		t.Errorf("resumed conversation holds %q, want a's last turn", got)
	}
	if !a.OpenElsewhere(held) {
		t.Error("the conversation b opened is not marked open for a")
	}
}

// TestListFollowsAnotherManager checks that List shows the conversations another manager
// creates, renames and deletes.
func TestListFollowsAnotherManager(t *testing.T) {
	dir := t.TempDir()
	a := LoadSessionManager(dir, testModel)
	b := LoadSessionManager(dir, testModel)
	names := func() []string {
		var out []string
		for _, c := range a.List() {
			out = append(out, c.Name)
		}
		return out
	}

	extra := b.New("extra")
	if got := names(); !slices.Contains(got, "extra") {
		t.Errorf("a lists %v, want b's new conversation", got)
	}
	if err := b.Rename(extra.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Contains(got, "renamed") || slices.Contains(got, "extra") {
		t.Errorf("a lists %v, want the new name", got)
	}
	if err := b.Delete(extra.ID); err != nil {
		t.Fatal(err)
	}
	if got := names(); slices.Contains(got, "renamed") {
		t.Errorf("a lists %v after b deleted the conversation", got)
	}
}

// TestRenameWritesAConversationNotOpen checks that renaming a conversation no manager has open
// writes it at once, keeping turns another manager saved after this one listed it.
func TestRenameWritesAConversationNotOpen(t *testing.T) {
	dir := t.TempDir()
	a := LoadSessionManager(dir, testModel)
	first := a.ActiveID
	say(a.Active(), "kept")
	a.New("")
	b := LoadSessionManager(dir, testModel) // opens first, which a left
	b.New("")                               // and leaves it, keeping its copy

	if _, err := a.Resume(first); err != nil {
		t.Fatal(err)
	}
	say(a.Active(), "later")
	a.New("") // saves first with both turns and leaves it

	if err := b.Rename(first, "named"); err != nil {
		t.Fatal(err)
	}
	if got := saved(t, dir, first); !slices.Equal(got, []string{"kept", "later"}) {
		t.Errorf("after the rename the file holds %q, want both turns", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "conversations", first+".json")); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := b.readConversation(first); c == nil || c.Name != "named" {
		t.Errorf("the rename was not written")
	}
}

// TestSecondManagerStartsWithTheStickyModel checks that a conversation started because every
// saved one is open elsewhere takes the model last chosen, not the configured default.
func TestSecondManagerStartsWithTheStickyModel(t *testing.T) {
	dir := t.TempDir()
	a := LoadSessionManager(dir, testModel)
	chosen := config.ModelRef{Provider: "p", Model: "chosen"}
	a.Active().Model = chosen
	a.SetDefaultModel(chosen)
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if got := LoadSessionManager(dir, testModel).Active().Model; got != chosen {
		t.Errorf("second manager's conversation uses %+v, want %+v", got, chosen)
	}
}

// TestDeletedIDsAreNotReused checks that a new conversation does not take the ID of a deleted
// one, also after a restart.
func TestDeletedIDsAreNotReused(t *testing.T) {
	dir := t.TempDir()
	sm := LoadSessionManager(dir, testModel) // 1
	sm.New("")                               // 2
	third := sm.New("")
	if err := sm.Delete(third.ID); err != nil {
		t.Fatal(err)
	}
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	if id := sm.New("").ID; id != "4" {
		t.Errorf("new conversation after deleting 3 got ID %s, want 4", id)
	}
	if err := sm.Delete("4"); err != nil {
		t.Fatal(err)
	}
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	closeManager(t, sm)
	if id := LoadSessionManager(dir, testModel).New("").ID; id != "5" {
		t.Errorf("new conversation after a restart got ID %s, want 5", id)
	}
}

// TestSaveDoesNotOverwriteAnotherManagersConversation checks that a conversation kept in memory,
// because its file could not be created, is not saved over a conversation another manager
// created under the same ID once the directory became writable.
func TestSaveDoesNotOverwriteAnotherManagersConversation(t *testing.T) {
	skipAsRoot(t)
	dir := t.TempDir()
	convDir := filepath.Join(dir, "conversations")
	if err := os.MkdirAll(convDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(convDir, 0o700) })
	a := LoadSessionManager(dir, testModel) // conversation 1 in memory only
	if err := os.Chmod(convDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := LoadSessionManager(dir, testModel)
	if b.ActiveID != a.ActiveID {
		t.Fatalf("b opened %s, want the same ID as a's unsaved %s", b.ActiveID, a.ActiveID)
	}
	say(b.Active(), "b's turn")
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	closeManager(t, b)

	say(a.Active(), "a's turn")
	if err := a.Save(); err == nil || !strings.Contains(err.Error(), "another hmj saved a conversation under its ID") {
		t.Errorf("Save = %v, want a refusal", err)
	}
	if got := saved(t, dir, b.ActiveID); !slices.Equal(got, []string{"b's turn"}) {
		t.Errorf("the file holds %q, want b's conversation untouched", got)
	}
}
