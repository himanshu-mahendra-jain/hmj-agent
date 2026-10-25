package session

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/config"
)

func TestSessionPersistence(t *testing.T) {
	sessionFile := t.TempDir()

	modelRef := config.ModelRef{Provider: "dummy", Model: "model-a"}

	// 1. Initial load with no file
	sm := LoadSessionManager(sessionFile, modelRef)
	if sm.Active().Name != "default" {
		t.Fatalf("expected default conversation, got %s", sm.Active().Name)
	}

	// Add messages to default
	sm.Active().Messages = append(sm.Active().Messages,
		chat.Message{Role: "user", Content: "hello"},
		chat.Message{Role: "assistant", Content: "world"},
	)

	// Create a second conversation
	c2 := sm.New("project-x")
	c2.Messages = append(c2.Messages,
		chat.Message{Role: "user", Content: "what is project x?"},
	)

	if err := sm.Save(); err != nil {
		t.Fatalf("failed to save sessions: %v", err)
	}
	closeManager(t, sm) // as hmj exits

	// 2. Reload sessions - should automatically resume project-x (last active)
	sm2 := LoadSessionManager(sessionFile, modelRef)
	if sm2.Active().ID != c2.ID || sm2.Active().Name != "project-x" {
		t.Fatalf("expected resumed active conversation to be project-x, got %s (id: %s)",
			sm2.Active().Name, sm2.Active().ID)
	}
	if sm2.Active().UserMessageCount() != 1 {
		t.Fatalf("expected 1 user message in resumed conversation, got %d", sm2.Active().UserMessageCount())
	}
	if len(sm2.List()) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(sm2.List()))
	}

	// 3. Switch to default, save, reload -> default should now be resumed
	if _, err := sm2.Resume("default"); err != nil {
		t.Fatalf("failed to resume default: %v", err)
	}
	if err := sm2.Save(); err != nil {
		t.Fatalf("failed to save after resume: %v", err)
	}
	closeManager(t, sm2)

	sm3 := LoadSessionManager(sessionFile, modelRef)
	if sm3.Active().Name != "default" {
		t.Fatalf("expected active to be default after resume, got %s", sm3.Active().Name)
	}
	if sm3.Active().UserMessageCount() != 1 {
		t.Fatalf("expected 1 user message in default, got %d", sm3.Active().UserMessageCount())
	}

	// 4. Test counter continuity
	c3 := sm3.New("project-y")
	if c3.ID != "3" {
		t.Fatalf("expected new conversation id to be 3, got %s", c3.ID)
	}
}

func TestSessionSaveIsCompactAndRoundTrips(t *testing.T) {
	path := t.TempDir()
	sm := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	conv := sm.Active()
	conv.Name = "rt-check"
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "hello"},
		chat.Message{Role: "assistant", Content: "world"},
	)
	if err := sm.Save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	closeManager(t, sm)
	data, err := os.ReadFile(filepath.Join(path, "conversations", conv.ID+".json"))
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if strings.Contains(string(data), "\n  ") {
		t.Error("expected compact JSON (no indentation)")
	}

	sm2 := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	convs := sm2.List()
	if len(convs) != 1 || convs[0].Name != "rt-check" {
		t.Fatalf("round-trip failed: %+v", convs)
	}
	if len(convs[0].Messages) != 3 { // system + 2 appended
		t.Fatalf("messages lost: %d", len(convs[0].Messages))
	}
	if convs[0].Messages[1].Content != "hello" || convs[0].Messages[2].Content != "world" {
		t.Errorf("message content corrupted: %+v", convs[0].Messages)
	}
}

func TestSessionRenameDeletePaths(t *testing.T) {
	sm := LoadSessionManager("", config.ModelRef{Provider: "p", Model: "m"})
	conv := sm.New("orig")

	if err := sm.Rename(conv.ID, "renamed"); err != nil {
		t.Errorf("rename failed: %v", err)
	}
	if sm.Conversations[conv.ID].Name != "renamed" {
		t.Errorf("rename not applied: %s", sm.Conversations[conv.ID].Name)
	}
	if err := sm.Rename("no-such-id", "x"); err == nil {
		t.Error("expected error renaming nonexistent conversation")
	}

	if err := sm.Delete(conv.ID); err != nil {
		t.Errorf("delete failed: %v", err)
	}
	if err := sm.Delete("no-such-id"); err == nil {
		t.Error("expected error deleting nonexistent conversation")
	}
}

func TestSessionCounterPersistsAcrossLoads(t *testing.T) {
	path := t.TempDir()

	sm := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	for range 3 {
		sm.New("conv")
	}
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	closeManager(t, sm)

	sm2 := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	conv := sm2.New("another")
	if conv.ID == "1" || conv.ID == "2" || conv.ID == "3" {
		t.Errorf("counter did not persist across loads; new conversation got duplicate id %q", conv.ID)
	}
}

func TestVerificationMandateInPrompt(t *testing.T) {
	prompt := DefaultSystemPrompt
	if !strings.Contains(prompt, "verify your work") {
		t.Errorf("expected prompt to contain 'verify your work', got: %s", prompt)
	}
	if !strings.Contains(prompt, "run_command") {
		t.Errorf("expected prompt to mention run_command, got: %s", prompt)
	}
	if !strings.Contains(prompt, "diagnose and fix") {
		t.Errorf("expected prompt to mention 'diagnose and fix', got: %s", prompt)
	}
}

func TestAgentsRulesPriorityOverReadme(t *testing.T) {
	tmpDir := t.TempDir()

	readmePath := filepath.Join(tmpDir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# Public README for Humans"), 0644); err != nil {
		t.Fatal(err)
	}

	agentsPath := filepath.Join(tmpDir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("# Agent Specific Rules"), 0644); err != nil {
		t.Fatal(err)
	}

	prompt := LoadSystemPrompt(tmpDir)
	if !strings.Contains(prompt, "Agent Specific Rules") {
		t.Errorf("expected prompt to load AGENTS.md, got: %s", prompt)
	}
	if strings.Contains(prompt, "Public README for Humans") {
		t.Errorf("expected prompt NOT to load README.md when AGENTS.md is present, got: %s", prompt)
	}
}

func TestLoadSystemPromptCapsAt160Lines(t *testing.T) {
	tmpDir := t.TempDir()

	var b strings.Builder
	for i := 1; i <= 250; i++ {
		b.WriteString("line ")
		b.WriteString(string(rune('0' + (i % 10))))
		b.WriteString("\n")
	}
	lines := strings.Split(b.String(), "\n")
	lines[159] = "UNIQUE_LINE_160"
	lines[160] = "UNIQUE_LINE_161"
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatal(err)
	}

	prompt := LoadSystemPrompt(tmpDir)
	if !strings.Contains(prompt, "UNIQUE_LINE_160") {
		t.Errorf("expected prompt to contain line 160")
	}
	if strings.Contains(prompt, "UNIQUE_LINE_161") {
		t.Errorf("expected prompt NOT to contain line 161 (capped at 160 lines)")
	}
	if !strings.Contains(DefaultSystemPrompt, "160 lines") {
		t.Errorf("expected DefaultSystemPrompt to mention 160 lines rule")
	}
	if !strings.Contains(DefaultSystemPrompt, "descriptive name that states its purpose") {
		t.Errorf("expected DefaultSystemPrompt to mention the descriptive file name rule")
	}
}

// TestLoadSessionManagerRecoversInvalidActiveID checks that a last-active ID naming no
// conversation opens the most recently changed one instead.
func TestLoadSessionManagerRecoversInvalidActiveID(t *testing.T) {
	dir := writeSessionDir(t, `{"activeId":"ghost","defaultModel":{"provider":"p","model":"m"}}`, map[string]string{
		"1": `{"id":"1","name":"one","model":{"provider":"p","model":"m"},"updatedAt":"2026-01-02T00:00:00Z"}`,
		"2": `{"id":"2","name":"two","model":{"provider":"p","model":"m"},"updatedAt":"2026-01-01T00:00:00Z"}`,
	})
	sm := LoadSessionManager(dir, config.ModelRef{Provider: "x", Model: "y"})
	if sm.ActiveID != "1" || sm.Active().Name != "one" || len(sm.Conversations) != 2 {
		t.Errorf("active=%q conversations=%d, want the most recently changed conversation", sm.ActiveID, len(sm.Conversations))
	}
	if names := []string{sm.List()[0].Name, sm.List()[1].Name}; !slices.Equal(names, []string{"one", "two"}) {
		t.Errorf("list = %v, want creation order, then IDs", names)
	}
}

func TestConversationNotFoundErrors(t *testing.T) {
	sm := LoadSessionManager("", config.ModelRef{Provider: "p", Model: "m"})
	_, resumeErr := sm.Resume("ghost")
	cases := map[string]struct {
		err  error
		want string
	}{
		"resume": {resumeErr, `conversation "ghost" not found`},
		"delete": {sm.Delete("ghost"), "conversation ghost not found"},
		"rename": {sm.Rename("ghost", "x"), "conversation ghost not found"},
	}
	for name, tc := range cases {
		if !errors.Is(tc.err, ErrConversationNotFound) {
			t.Errorf("%s: %v does not match ErrConversationNotFound", name, tc.err)
		}
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Errorf("%s: message = %v, want %q", name, tc.err, tc.want)
		}
	}
}

// Save writes conversations as they are: no tool output is shortened, in the active conversation
// or any other.
func TestSaveKeepsToolOutput(t *testing.T) {
	sm := LoadSessionManager(t.TempDir(), config.ModelRef{Provider: "p", Model: "m"})
	bulky := strings.Repeat("output line\n", 100)
	fill := func(c *Conversation) {
		c.Messages = append(c.Messages,
			chat.Message{Role: "user", Content: "run it"},
			chat.Message{Role: "tool", ToolCallID: "c1", Content: bulky},
			chat.Message{Role: "user", Content: "next"},
		)
	}
	inactive := sm.Active()
	fill(inactive)
	active := sm.New("current")
	fill(active)

	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	if inactive.Messages[2].Content != bulky || active.Messages[2].Content != bulky {
		t.Error("tool output was shortened on save")
	}
}

// Credentials in the conversation are saved as they are, so a resumed conversation is the one the
// model produced.
func TestSaveKeepsCredentials(t *testing.T) {
	path := t.TempDir()
	sm := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	const key = "sk-proj-0123456789abcdefghijklmn"
	conv := sm.Active()
	conv.Messages = append(conv.Messages, chat.Message{Role: "tool", ToolCallID: "c1", Content: "OPENAI_API_KEY=" + key})
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	closeManager(t, sm)
	if got := LoadSessionManager(path, config.ModelRef{}).Active().Messages; got[len(got)-1].Content != "OPENAI_API_KEY="+key {
		t.Errorf("loaded tool output = %q, want it as saved", got[len(got)-1].Content)
	}
}

// TestAutoTitledFollowsNameSource checks that an automatic title is marked as such and that a user
// rename clears the mark.
func TestAutoTitledFollowsNameSource(t *testing.T) {
	sm := LoadSessionManager(t.TempDir(), config.ModelRef{Provider: "dummy", Model: "model-a"})
	conv := sm.Active()
	if conv.AutoTitled {
		t.Fatalf("a new conversation is marked as auto-titled")
	}

	conv.MaybeAutoTitle("fix the login redirect bug")
	if !conv.AutoTitled || conv.Name != GenerateConversationTitle("fix the login redirect bug") {
		t.Fatalf("MaybeAutoTitle: AutoTitled=%v Name=%q, want an auto-titled heuristic name", conv.AutoTitled, conv.Name)
	}

	if err := sm.Rename(conv.ID, "My name"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if conv.AutoTitled {
		t.Errorf("Rename left AutoTitled set")
	}

	conv.MaybeAutoTitle("another prompt")
	if conv.Name != "My name" || conv.AutoTitled {
		t.Errorf("MaybeAutoTitle replaced a user name: Name=%q AutoTitled=%v", conv.Name, conv.AutoTitled)
	}
}

// TestSessionKeepsToolResultImages checks that a tool result's picture references survive a
// save and a load, and that the file holds the reference, not the picture.
func TestSessionKeepsToolResultImages(t *testing.T) {
	path := t.TempDir()
	sm := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"})
	img := chat.Image{File: strings.Repeat("ab", 32) + ".png", MediaType: "image/png", Width: 900, Height: 2000, Source: "shot.png"}
	sm.Active().Messages = append(sm.Active().Messages, chat.Message{Role: "tool", ToolCallID: "c1", Content: "shot.png", Images: []chat.Image{img}})
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	closeManager(t, sm)
	data, _ := os.ReadFile(filepath.Join(path, "conversations", sm.ActiveID+".json"))
	if !strings.Contains(string(data), `"images":[{"file":"`+img.File+`","media_type":"image/png","width":900,"height":2000,"source":"shot.png"}]`) {
		t.Errorf("saved file lacks the image reference:\n%s", data)
	}
	msgs := LoadSessionManager(path, config.ModelRef{Provider: "p", Model: "m"}).List()[0].Messages
	if got := msgs[len(msgs)-1].Images; len(got) != 1 || got[0] != img {
		t.Errorf("loaded images = %+v, want %+v", got, img)
	}
}

// closeManager releases sm's lock on its conversation, as hmj does when it exits, so a manager
// loaded next in the same test can open it.
func closeManager(t *testing.T, sm *SessionManager) {
	t.Helper()
	if err := sm.Close(); err != nil {
		t.Fatal(err)
	}
}
