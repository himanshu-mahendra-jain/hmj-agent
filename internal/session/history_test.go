package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/config"
)

// savedConversation loads a session manager saving to a temporary directory and returns it with
// its active conversation.
func savedConversation(t *testing.T) (*SessionManager, *Conversation, string) {
	t.Helper()
	path := t.TempDir()
	sm := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	return sm, sm.Active(), path
}

// addTurn appends a question and its answer to conv.
func addTurn(conv *Conversation, n int) {
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: fmt.Sprintf("question %d", n)},
		chat.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", n)},
	)
}

// compactAll compacts every message of conv, failing the test on an error.
func compactAll(t *testing.T, conv *Conversation, summary string) {
	t.Helper()
	if err := conv.ApplyCompaction(summaryMessage(summary), len(conv.Messages)); err != nil {
		t.Fatal(err)
	}
}

// prompts returns the content of the user messages in msgs.
func prompts(msgs []chat.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

// reload saves sm and loads its session directory again, as a restart does.
func reload(t *testing.T, sm *SessionManager, path string) (*SessionManager, *Conversation) {
	t.Helper()
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	closeManager(t, sm)
	sm = LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	return sm, sm.Active()
}

// TestCompactionKeepsReplacedTurnsForHistory checks that the messages a compaction replaces are
// kept in the history file, without their reasoning blocks or pictures, that they are still there
// after a restart, and that a second compaction adds its turns without the first compaction.
func TestCompactionKeepsReplacedTurnsForHistory(t *testing.T) {
	sm, conv, path := savedConversation(t)
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "question 1"},
		chat.Message{Role: "assistant", Content: "reading",
			Blocks:    []chat.Block{{Type: "thinking", Text: "secret", Signature: "sig"}},
			ToolCalls: []chat.ToolCall{{ID: "c1", Type: "function", Function: chat.ToolCallFunction{Name: "read_file", Arguments: `{"path":"x"}`}}}},
		chat.Message{Role: "tool", ToolCallID: "c1", Content: "file text", Images: []chat.Image{{File: "pic.png"}}},
		chat.Message{Role: "assistant", Content: "answer 1"},
	)
	addTurn(conv, 2)
	compactAll(t, conv, "first summary")
	if conv.ArchivedBytes == 0 {
		t.Fatal("ArchivedBytes is 0 after a compaction")
	}

	sm, conv = reload(t, sm, path)
	earlier, err := conv.EarlierMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(earlier) != 6 || earlier[0].Content != "question 1" || earlier[5].Content != "answer 2" {
		t.Fatalf("earlier messages = %+v, want the two turns without the system prompt", earlier)
	}
	if call := earlier[1]; call.Blocks != nil || len(call.ToolCalls) != 1 || call.ToolCalls[0].Function.Arguments != `{"path":"x"}` {
		t.Errorf("tool call kept as %+v, want its call without reasoning blocks", call)
	}
	if res := earlier[2]; res.Images != nil || res.ToolCallID != "c1" || res.Content != "file text" {
		t.Errorf("tool result kept as %+v, want its text without pictures", res)
	}

	addTurn(conv, 3)
	compactAll(t, conv, "second summary")
	_, conv = reload(t, sm, path)
	earlier, err = conv.EarlierMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := prompts(earlier); len(got) != 3 || got[2] != "question 3" {
		t.Errorf("prompts kept = %q, want questions 1 to 3", got)
	}
	for _, m := range earlier {
		if m.CompactionBlock() != nil {
			t.Errorf("the first compaction was kept: %+v", m)
		}
	}
}

// TestHistoryIgnoresAWriteTheSessionFileMissed checks that messages written to the history file
// by a compaction the conversation file never recorded, as when hmj stops before saving, are not shown
// and are overwritten by the next compaction, so no turn appears twice. A history file that lost
// its end takes the next turns after what is left.
func TestHistoryIgnoresAWriteTheSessionFileMissed(t *testing.T) {
	sm, conv, path := savedConversation(t)
	addTurn(conv, 1)
	compactAll(t, conv, "s1")
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	addTurn(conv, 2)
	compactAll(t, conv, "s2") // written to the history file, never saved
	closeManager(t, sm)       // hmj stops

	conv = LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"}).Active()
	earlier, err := conv.EarlierMessages()
	if err != nil {
		t.Fatal(err)
	}
	if got := prompts(earlier); len(got) != 1 || got[0] != "question 1" {
		t.Fatalf("prompts after the missed save = %q, want question 1 only", got)
	}
	addTurn(conv, 2) // the turn is lost with the unsaved conversation; ask it again
	compactAll(t, conv, "s2 again")
	earlier, _ = conv.EarlierMessages()
	if got := prompts(earlier); len(got) != 2 || got[1] != "question 2" {
		t.Errorf("prompts after compacting again = %q, want questions 1 and 2 once each", got)
	}

	if err := os.Truncate(conv.HistoryPath, 0); err != nil {
		t.Fatal(err)
	}
	addTurn(conv, 3)
	compactAll(t, conv, "s3")
	earlier, err = conv.EarlierMessages()
	if got := prompts(earlier); err != nil || len(got) != 1 || got[0] != "question 3" {
		t.Errorf("prompts after the file was emptied = %q (%v), want question 3", got, err)
	}
}

// TestCompactionAppliedWhenHistoryCannotBeWritten checks that a history file that cannot be
// written does not stop the compaction: it is applied, and the error says the earlier turns were
// not kept.
func TestCompactionAppliedWhenHistoryCannotBeWritten(t *testing.T) {
	conv := chatTurns(2)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	conv.HistoryPath = filepath.Join(blocker, "c.jsonl")

	err := conv.ApplyCompaction(summaryMessage("s"), len(conv.Messages))
	if !errors.Is(err, ErrHistoryNotSaved) {
		t.Fatalf("ApplyCompaction = %v, want ErrHistoryNotSaved", err)
	}
	if len(conv.Messages) != 2 || conv.Messages[1].CompactionBlock() == nil {
		t.Errorf("messages = %+v, want the compaction applied", conv.Messages)
	}
	if conv.ArchivedBytes != 0 {
		t.Errorf("ArchivedBytes = %d after a failed write, want 0", conv.ArchivedBytes)
	}
}

// TestUnsavedConversationKeepsHistoryInMemory checks that a conversation without a session file
// keeps the replaced turns for as long as it runs.
func TestUnsavedConversationKeepsHistoryInMemory(t *testing.T) {
	conv := chatTurns(2)
	compactAll(t, conv, "s")
	earlier, err := conv.EarlierMessages()
	if got := prompts(earlier); err != nil || len(got) != 2 || len(earlier) != 4 {
		t.Errorf("earlier messages = %+v (%v), want the two turns", earlier, err)
	}
	if fresh, err := chatTurns(1).EarlierMessages(); fresh != nil || err != nil {
		t.Errorf("a conversation never compacted has earlier messages %+v (%v)", fresh, err)
	}
}

// TestHistoryFilesFollowTheirConversation checks that deleting a conversation removes its
// history file, and that loading the session file removes the history file of a conversation it
// does not hold once it is an hour old, keeping a newer one that another hmj may have just made.
func TestHistoryFilesFollowTheirConversation(t *testing.T) {
	sm, conv, path := savedConversation(t)
	addTurn(conv, 1)
	compactAll(t, conv, "s")
	file := conv.HistoryPath
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	keep := sm.New("other")
	if err := sm.Delete(conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("history file of a deleted conversation: %v, want it removed", err)
	}

	dir := filepath.Dir(file)
	old, recent, kept := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "recent.jsonl"), keep.HistoryPath
	for _, p := range []string{old, recent, kept} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hourAgo := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, hourAgo, hourAgo)
	os.Chtimes(kept, hourAgo, hourAgo)
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("an old orphan history file was kept: %v", err)
	}
	for _, p := range []string{recent, kept} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
}
