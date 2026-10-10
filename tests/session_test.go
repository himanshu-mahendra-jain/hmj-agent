package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
)

func TestVerificationMandateInPrompt(t *testing.T) {
	prompt := session.DefaultSystemPrompt
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

	prompt := session.LoadSystemPrompt(tmpDir)
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
	// Give unique markers to line 160 and 161
	lines := strings.Split(b.String(), "\n")
	lines[159] = "UNIQUE_LINE_160"
	lines[160] = "UNIQUE_LINE_161"
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatal(err)
	}

	prompt := session.LoadSystemPrompt(tmpDir)
	if !strings.Contains(prompt, "UNIQUE_LINE_160") {
		t.Errorf("expected prompt to contain line 160")
	}
	if strings.Contains(prompt, "UNIQUE_LINE_161") {
		t.Errorf("expected prompt NOT to contain line 161 (capped at 160 lines)")
	}
	if !strings.Contains(session.DefaultSystemPrompt, "160 lines") {
		t.Errorf("expected DefaultSystemPrompt to mention 160 lines rule")
	}
}

func TestCorruptSessionPreserved(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(sessionFile, []byte("{invalid json corrupt data"), 0o600); err != nil {
		t.Fatal(err)
	}

	sm := session.LoadSessionManager(sessionFile, config.ModelRef{Provider: "p", Model: "m"})
	if sm == nil {
		t.Fatal("expected non-nil SessionManager")
	}

	// Verify that a .corrupt. file was created
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var foundCorrupt bool
	for _, f := range files {
		if strings.Contains(f.Name(), ".corrupt.") {
			foundCorrupt = true
			break
		}
	}
	if !foundCorrupt {
		t.Error("expected corrupt session file to be preserved as .corrupt.<ts>")
	}

	// Saving should succeed and write a valid new file
	if err := sm.Save(); err != nil {
		t.Fatalf("sm.Save() failed: %v", err)
	}
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "activeId") {
		t.Errorf("expected valid session JSON after Save, got: %s", string(data))
	}
}

// Condense keeps the system prompt and the recent turns word for word, with the summary in
// place of the older turns, whatever characters they hold.
func TestCondenseKeepsRecentTurns(t *testing.T) {
	conv := &session.Conversation{
		ID:   "test-condense",
		Name: "test-condense",
		Messages: []chat.Message{
			{Role: "system", Content: "System prompt"},
		},
	}
	for i := 1; i <= 8; i++ {
		userPrompt := fmt.Sprintf("Turn %d user question: %s", i, strings.Repeat("こんにちは世界🌟", 20))
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: userPrompt},
			chat.Message{
				Role:      "assistant",
				Content:   fmt.Sprintf("Turn %d assistant response:\n%s", i, strings.Repeat("detailed analysis and code implementation details\n", 20)),
				ToolCalls: []chat.ToolCall{{Function: chat.ToolCallFunction{Name: "read_file"}}},
			},
			chat.Message{Role: "tool", Content: fmt.Sprintf("Turn %d tool output:\n%s", i, strings.Repeat("file contents line\n", 30))},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("Turn %d final conclusion with explanations", i)},
		)
	}
	origCount := len(conv.Messages)

	summarize := func(_ context.Context, history []chat.Message) (string, error) {
		if history[0].Content != "System prompt" || len(history) != 1+5*4 {
			t.Errorf("summarizer got %d messages starting %q, want the system prompt and 5 turns of 4 messages", len(history), history[0].Content)
		}
		return "The user asked eight questions; 日本語 answers were given.", nil
	}
	condensed, err := conv.Condense(context.Background(), 3, summarize)
	if err != nil || !condensed {
		t.Fatalf("Condense = (%v, %v), want (true, nil) for more than 6 turns", condensed, err)
	}

	if conv.Messages[0].Role != "system" {
		t.Errorf("expected system message at index 0, got %s", conv.Messages[0].Role)
	}
	summaryMsg := conv.Messages[1]
	if summaryMsg.Role != "user" || !strings.HasPrefix(summaryMsg.Content, session.SummaryPrefix) || !strings.HasSuffix(summaryMsg.Content, "answers were given.") {
		t.Errorf("expected the summary in message 1, got role=%s content=%s", summaryMsg.Role, summaryMsg.Content)
	}
	if !utf8.ValidString(summaryMsg.Content) {
		t.Error("expected summary to be valid UTF-8, but found invalid byte sequences")
	}
	if conv.Messages[2].Role != "assistant" {
		t.Errorf("expected an assistant acknowledgment in message 2, got: %v", conv.Messages[2])
	}

	// 1 summary user message + 3 recent user messages
	userCount := 0
	for _, m := range conv.Messages {
		if m.Role == "user" {
			userCount++
		}
	}
	if userCount != 4 {
		t.Errorf("expected 4 user messages after condensing, got %d", userCount)
	}

	lastUserMsg := conv.Messages[len(conv.Messages)-4]
	expectedTurn8 := fmt.Sprintf("Turn 8 user question: %s", strings.Repeat("こんにちは世界🌟", 20))
	if lastUserMsg.Content != expectedTurn8 {
		t.Errorf("expected Turn 8 to be preserved, got: %s", lastUserMsg.Content)
	}
	if len(conv.Messages) >= origCount {
		t.Errorf("expected message count to shrink, went from %d to %d", origCount, len(conv.Messages))
	}
}

func TestSessionResumeMatrix(t *testing.T) {
	sm := session.NewSessionManager(config.ModelRef{Provider: "mock", Model: "mock-model"})
	// NewSessionManager seeds a "default" conversation; three more follow.
	sm.New("alpha")
	sm.New("beta project")
	sm.New("gamma")

	// Empty query
	if _, err := sm.Resume("  "); err == nil {
		t.Error("expected error for empty query")
	}
	// Exact ID (IDs are "1".."4"; 3 = "beta project")
	conv, err := sm.Resume("3")
	if err != nil || conv.Name != "beta project" {
		t.Errorf("resume by ID failed: %v", err)
	}
	// Exact name (case-insensitive)
	conv, err = sm.Resume("ALPHA")
	if err != nil || conv.Name != "alpha" {
		t.Errorf("resume by name failed: %v", err)
	}
	// 1-based index resolves identically here
	conv, err = sm.Resume("4")
	if err != nil || conv.Name != "gamma" {
		t.Errorf("resume by index failed: %v", err)
	}
	// Out-of-range index falls through to fuzzy matching -> error
	if _, err := sm.Resume("99"); err == nil {
		t.Error("expected error for out-of-range index")
	}
	// Partial name match
	conv, err = sm.Resume("beta")
	if err != nil || conv.Name != "beta project" {
		t.Errorf("resume by partial name failed: %v", err)
	}
	// No match
	if _, err := sm.Resume("zzz-nothing"); err == nil {
		t.Error("expected error for no match")
	}
}

func TestConversationUserMessageCount(t *testing.T) {
	sm := session.NewSessionManager(config.ModelRef{Provider: "mock", Model: "mock-model"})
	conv := sm.New("counter")
	conv.Messages = []chat.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
	}
	if got := conv.UserMessageCount(); got != 2 {
		t.Errorf("want 2 user messages, got %d", got)
	}
}

func TestSessionRenameDeletePaths(t *testing.T) {
	sm := session.NewSessionManager(config.ModelRef{Provider: "p", Model: "m"})
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

func newTestConversation(id, name string) *session.Conversation {
	return &session.Conversation{
		ID:       id,
		Name:     name,
		Model:    testModelRef(),
		Messages: []chat.Message{},
	}
}

// TestSessionCounterPersistsAcrossLoads is a regression test: conversation IDs
// must not restart from 1 after a restart (which would collide with old IDs).
func TestSessionCounterPersistsAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")

	sm := session.LoadSessionManager(path, testModelRef())
	for range 3 {
		sm.New("conv")
	}
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}

	sm2 := session.LoadSessionManager(path, testModelRef())
	conv := sm2.New("another")
	if conv.ID == "1" || conv.ID == "2" || conv.ID == "3" {
		t.Errorf("counter did not persist across loads; new conversation got duplicate id %q", conv.ID)
	}
}

// TestSessionCorruptFileRecoversDefault verifies a corrupt session file is
// safely recovered as a clean default session in this alpha app without panicking.
func TestSessionCorruptFileRecoversDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	corrupt := []byte(`{"activeId": "x", "conversations": {TRUNCATED`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	sm := session.LoadSessionManager(path, testModelRef())
	if sm.Active() == nil || sm.Active().Name != "default" {
		t.Fatalf("expected clean default session recovery, got: %+v", sm.Active())
	}
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
}

// TestSessionConcurrentAccess exercises the session mutex under -race.
func TestSessionConcurrentAccess(t *testing.T) {
	sm := session.LoadSessionManager("", testModelRef())

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 25 {
				sm.New(fmt.Sprintf("conv-%d", i))
				_ = sm.Active()
				_ = sm.List()
				if err := sm.Save(); err != nil {
					t.Errorf("concurrent Save failed: %v", err)
				}
			}
		})
	}
	wg.Wait()
}

// TestCompactMarksPrunedFlag verifies the explicit Pruned flag replaces
// content sniffing for double-prune avoidance.
func TestCompactMarksPrunedFlag(t *testing.T) {
	c := newTestConversation("c1", "test")

	big := strings.Repeat("line\n", 200)
	// Two old tool outputs (beyond the 2-call keep window) plus two fresh ones.
	c.Messages = append(c.Messages,
		chat.Message{Role: "user", Content: "go"},
		chat.Message{Role: "tool", Content: big},
		chat.Message{Role: "tool", Content: big},
		chat.Message{Role: "user", Content: "next turn"},
		chat.Message{Role: "tool", Content: big},
		chat.Message{Role: "tool", Content: big},
	)

	p1, saved1 := c.Compact()
	if p1 == 0 || saved1 == 0 {
		t.Fatal("expected first compaction to prune old output")
	}
	if !c.Pruned {
		t.Error("expected Pruned flag to be set after compaction")
	}

	// Second pass must be a no-op: everything eligible is already compacted.
	p2, saved2 := c.Compact()
	if p2 != 0 || saved2 != 0 {
		t.Errorf("expected second compaction to be a no-op, got pruned=%d saved=%d", p2, saved2)
	}
}

// TestSessionStoragePathAccessors covers the storage path getter/setter.
func TestSessionStoragePathAccessors(t *testing.T) {
	sm := session.LoadSessionManager("", testModelRef())
	if sm.StoragePath() != "" {
		t.Errorf("expected empty storage path, got %q", sm.StoragePath())
	}
	sm.SetStoragePath("/tmp/hmj-test-session.json")
	if sm.StoragePath() != "/tmp/hmj-test-session.json" {
		t.Errorf("SetStoragePath did not take effect: %q", sm.StoragePath())
	}
}

func TestConversationAutoTitleRenameDelete(t *testing.T) {
	modelRef := config.ModelRef{Provider: "dummy", Model: "model-a"}
	sm := session.NewSessionManager(modelRef)
	conv := sm.Active()

	// 1. Test auto-titling
	conv.MaybeAutoTitle("Fix database deadlock when updating user accounts")
	if !strings.HasPrefix(conv.Name, "Fix database deadlock") {
		t.Fatalf("expected auto-titled conversation, got %q", conv.Name)
	}

	// Shouldn't overwrite custom name
	conv.MaybeAutoTitle("Another prompt")
	if !strings.HasPrefix(conv.Name, "Fix database deadlock") {
		t.Fatalf("auto-title overwrote existing non-default title, got %q", conv.Name)
	}

	// 2. Test rename
	err := sm.Rename(conv.ID, "New Renamed Title")
	if err != nil {
		t.Fatalf("failed to rename: %v", err)
	}
	if conv.Name != "New Renamed Title" {
		t.Fatalf("expected New Renamed Title, got %q", conv.Name)
	}

	// 3. Test delete
	c2 := sm.New("temporary-chat")
	if len(sm.List()) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(sm.List()))
	}
	err = sm.Delete(c2.ID)
	if err != nil {
		t.Fatalf("failed to delete c2: %v", err)
	}
	if len(sm.List()) != 1 {
		t.Fatalf("expected 1 conversation after delete, got %d", len(sm.List()))
	}
	if sm.Active().ID != conv.ID {
		t.Fatalf("expected active conversation to revert to conv, got %s", sm.Active().ID)
	}

	// Deleting the last conversation should create a new default
	err = sm.Delete(conv.ID)
	if err != nil {
		t.Fatalf("failed to delete last conversation: %v", err)
	}
	if len(sm.List()) != 1 || sm.Active().Name != "default" {
		t.Fatalf("expected fresh default conversation after deleting all, got %v", sm.List())
	}
}

func TestPruneOldToolOutputs(t *testing.T) {
	conv := &session.Conversation{
		ID:   "test-prune",
		Name: "test-prune",
	}

	// Generate a 100-line tool output
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d: package main function declaration or test log", i))
	}
	largeOutput := strings.Join(lines, "\n")

	// Turn 1: user prompt + assistant tool call + large tool output + assistant reply
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "Read the large file"},
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_1"}}},
		chat.Message{Role: "tool", ToolCallID: "call_1", Content: largeOutput},
		chat.Message{Role: "assistant", Content: "I have read the file."},
	)

	// Turn 2: user prompt + small tool output
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "Now do something small"},
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_2"}}},
		chat.Message{Role: "tool", ToolCallID: "call_2", Content: "ok, done"},
		chat.Message{Role: "assistant", Content: "Completed small step."},
	)

	// Turn 3: current user turn
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "What is next?"},
	)

	// Pruning keeping 1 recent turn (Turn 2 + 3 are recent, Turn 1 is old)
	prunedCount, bytesSaved := conv.PruneOldToolOutputs(1)
	if prunedCount != 1 {
		t.Fatalf("expected 1 pruned tool output, got %d", prunedCount)
	}
	if bytesSaved <= 0 {
		t.Fatalf("expected bytesSaved > 0, got %d", bytesSaved)
	}

	// Verify Turn 1 tool output was pruned and contains the omission notice
	prunedContent := conv.Messages[2].Content
	if !strings.Contains(prunedContent, "lines pruned for context efficiency") {
		t.Fatalf("expected prune marker in tool output, got:\n%s", prunedContent)
	}
	if !strings.HasPrefix(prunedContent, "line 1:") {
		t.Fatalf("expected head preserved, got:\n%s", prunedContent)
	}
	if !strings.HasSuffix(prunedContent, "line 100: package main function declaration or test log") {
		t.Fatalf("expected tail preserved, got:\n%s", prunedContent)
	}

	// Verify Turn 2 tool output remains untouched
	if conv.Messages[6].Content != "ok, done" {
		t.Fatalf("expected Turn 2 tool output to be untouched, got %q", conv.Messages[6].Content)
	}

	// Verify idempotent (second run shouldn't double prune or corrupt)
	p2, s2 := conv.PruneOldToolOutputs(1)
	if p2 != 0 || s2 != 0 {
		t.Fatalf("expected second prune to be idempotent, got count=%d, saved=%d", p2, s2)
	}
	if conv.Messages[2].Content != prunedContent {
		t.Fatalf("content mutated on second prune run")
	}

	// Test long single-line string compaction
	conv2 := &session.Conversation{ID: "test-singleline"}
	longLine := "data:" + strings.Repeat("A1b2C3d4E5", 200) // 2005 chars
	conv2.Messages = append(conv2.Messages,
		chat.Message{Role: "user", Content: "Run minified"},
		chat.Message{Role: "tool", Content: longLine},
		chat.Message{Role: "user", Content: "Next turn"},
	)
	pCount, bSaved := conv2.PruneOldToolOutputs(1)
	if pCount != 1 || bSaved <= 0 {
		t.Fatalf("expected 1 single-line pruned, got count=%d saved=%d", pCount, bSaved)
	}
	if !strings.Contains(conv2.Messages[1].Content, "bytes pruned for context efficiency") {
		t.Fatalf("expected byte prune marker, got %s", conv2.Messages[1].Content)
	}
}

func TestCompactionMultiTurnRegression(t *testing.T) {
	conv := &session.Conversation{
		ID:   "test-multiturn-compaction",
		Name: "test-multiturn-compaction",
	}

	generateLargeOutput := func() string {
		var lines []string
		for i := 1; i <= 100; i++ {
			lines = append(lines, fmt.Sprintf("line %d: large debug log payload content", i))
		}
		return strings.Join(lines, "\n")
	}

	// Turn 1
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "Turn 1 prompt"},
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_t1"}}},
		chat.Message{Role: "tool", ToolCallID: "call_t1", Content: generateLargeOutput()},
		chat.Message{Role: "assistant", Content: "Turn 1 done"},
	)

	// Turn 2
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "Turn 2 prompt"},
	)

	// Pass 1: Turn 1 should be pruned
	p1, s1 := conv.Compact()
	if p1 != 1 || s1 <= 0 {
		t.Fatalf("pass 1 failed: expected 1 pruned, got %d (saved %d)", p1, s1)
	}
	if !conv.Pruned {
		t.Fatal("expected conv.Pruned to be true after pass 1")
	}

	// Turn 2 adds a new bulky tool output
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_t2"}}},
		chat.Message{Role: "tool", ToolCallID: "call_t2", Content: generateLargeOutput()},
		chat.Message{Role: "assistant", Content: "Turn 2 done"},
	)

	// Turn 3 begins
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "Turn 3 prompt"},
	)

	// Pass 2: Turn 2's bulky output should ALSO be pruned despite conv.Pruned == true!
	p2, s2 := conv.Compact()
	if p2 != 1 || s2 <= 0 {
		t.Fatalf("pass 2 failed (one-shot bug regression): expected 1 pruned, got %d (saved %d)", p2, s2)
	}
}

func TestPruneInFlightToolOutputs(t *testing.T) {
	conv := &session.Conversation{
		ID:   "test-inflight",
		Name: "test-inflight",
	}

	makeOutput := func(prefix string, n int) string {
		var lines []string
		for i := 1; i <= n; i++ {
			lines = append(lines, fmt.Sprintf("%s line %d: details and logs", prefix, i))
		}
		return strings.Join(lines, "\n")
	}

	// In a single turn, the LLM makes 4 successive tool calls
	conv.Messages = append(conv.Messages,
		chat.Message{Role: "user", Content: "Perform multi-step task"},
		// Step 1: bulky tool 1
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_1"}}},
		chat.Message{Role: "tool", ToolCallID: "call_1", Content: makeOutput("tool1", 50)},
		// Step 2: bulky tool 2
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_2"}}},
		chat.Message{Role: "tool", ToolCallID: "call_2", Content: makeOutput("tool2", 50)},
		// Step 3: bulky tool 3 (recent)
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_3"}}},
		chat.Message{Role: "tool", ToolCallID: "call_3", Content: makeOutput("tool3", 50)},
		// Step 4: bulky tool 4 (most recent)
		chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "call_4"}}},
		chat.Message{Role: "tool", ToolCallID: "call_4", Content: makeOutput("tool4", 50)},
	)

	// Keep the last 2 tool calls intact: tool 1 and tool 2 should be pruned; tool 3 and tool 4 must stay untouched
	prunedCount, bytesSaved := conv.PruneInFlightToolOutputs(2)
	if prunedCount != 2 {
		t.Fatalf("expected 2 pruned tool outputs, got %d", prunedCount)
	}
	if bytesSaved <= 0 {
		t.Fatalf("expected positive bytesSaved, got %d", bytesSaved)
	}

	// Tool 1 and Tool 2 must have prune markers
	if !strings.Contains(conv.Messages[2].Content, "lines pruned for context efficiency") {
		t.Fatalf("expected tool 1 to be pruned, got:\n%s", conv.Messages[2].Content)
	}
	if !strings.Contains(conv.Messages[4].Content, "lines pruned for context efficiency") {
		t.Fatalf("expected tool 2 to be pruned, got:\n%s", conv.Messages[4].Content)
	}

	// Tool 3 and Tool 4 must remain untouched in full fidelity
	if strings.Contains(conv.Messages[6].Content, "lines pruned for context efficiency") {
		t.Fatalf("tool 3 should remain unpruned, got:\n%s", conv.Messages[6].Content)
	}
	if strings.Contains(conv.Messages[8].Content, "lines pruned for context efficiency") {
		t.Fatalf("tool 4 should remain unpruned, got:\n%s", conv.Messages[8].Content)
	}

	// Test idempotent: second call shouldn't prune again
	p2, s2 := conv.PruneInFlightToolOutputs(2)
	if p2 != 0 || s2 != 0 {
		t.Fatalf("expected 0 additional prunes, got %d, %d", p2, s2)
	}

	// Test conv.Compact()
	p3, _ := conv.Compact()
	if p3 != 0 {
		t.Fatalf("expected 0 additional prunes on already compacted conversation, got %d", p3)
	}
}

func TestLoadSystemPromptFromReadme(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Without README, returns DefaultSystemPrompt
	promptDefault := session.LoadSystemPrompt(tmpDir)
	if promptDefault != session.DefaultSystemPrompt {
		t.Fatalf("expected DefaultSystemPrompt, got %s", promptDefault)
	}

	// 2. With README.md, returns prompt augmented with README content
	readmeContent := "# Project Guidelines\n- Always use latest Go\n- No third-party modules"
	readmePath := filepath.Join(tmpDir, "README.md")
	if err := os.WriteFile(readmePath, []byte(readmeContent), 0644); err != nil {
		t.Fatalf("failed to write README.md: %v", err)
	}

	promptAugmented := session.LoadSystemPrompt(tmpDir)
	if !strings.Contains(promptAugmented, session.DefaultSystemPrompt) {
		t.Errorf("expected prompt to contain DefaultSystemPrompt")
	}
	if !strings.Contains(promptAugmented, "Project Guidelines") {
		t.Errorf("expected prompt to contain README content, got %s", promptAugmented)
	}
	if !strings.Contains(promptAugmented, "Always use latest Go") {
		t.Errorf("expected prompt to contain 'Always use latest Go', got %s", promptAugmented)
	}

	// 3. Test SyncSystemPrompt on Conversation
	conv := &session.Conversation{}
	conv.SyncSystemPrompt(promptAugmented)
	if len(conv.Messages) != 1 || conv.Messages[0].Role != "system" {
		t.Fatalf("expected 1 system message, got %+v", conv.Messages)
	}
	if conv.Messages[0].Content != promptAugmented {
		t.Errorf("expected system message content to match promptAugmented")
	}

	// Updating prompt replaces index 0
	conv.SyncSystemPrompt("new prompt")
	if len(conv.Messages) != 1 || conv.Messages[0].Content != "new prompt" {
		t.Fatalf("expected updated system message, got %+v", conv.Messages)
	}
}

// Repeated corruption keeps only the newest damaged copies, so backups don't pile up.
func TestCorruptSessionBackupsArePruned(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "sessions.json")
	// Seven older backups, oldest first by timestamp.
	for i := range 7 {
		old := fmt.Sprintf("%s.corrupt.2026010100000%d", sessionFile, i)
		if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(sessionFile, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	session.LoadSessionManager(sessionFile, config.ModelRef{Provider: "p", Model: "m"})

	backups, err := filepath.Glob(sessionFile + ".corrupt.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 5 {
		t.Fatalf("kept %d backups, want 5: %v", len(backups), backups)
	}
	slices.Sort(backups)
	if !strings.HasSuffix(backups[0], ".corrupt.20260101000003") {
		t.Errorf("oldest kept backup = %s, want the 4th oldest (…03)", backups[0])
	}
	data, err := os.ReadFile(backups[len(backups)-1])
	if err != nil || string(data) != "{broken" {
		t.Errorf("newest backup should be the file just moved aside, got %q (%v)", data, err)
	}
}

// turnsConversation builds a conversation of n turns whose replies are replySize bytes.
func turnsConversation(n, replySize int) *session.Conversation {
	conv := &session.Conversation{Messages: []chat.Message{{Role: "system", Content: "sys"}}}
	for i := range n {
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("request %d", i)},
			chat.Message{Role: "assistant", Content: strings.Repeat("r", replySize)})
	}
	return conv
}

func TestFitContext(t *testing.T) {
	ctx := context.Background()
	summarize := func(_ context.Context, history []chat.Message) (string, error) {
		return fmt.Sprintf("summary of %d messages", len(history)), nil
	}

	t.Run("within budget is untouched", func(t *testing.T) {
		conv := turnsConversation(4, 4000)
		before := slices.Clone(conv.Messages)
		if changed, err := conv.FitContext(ctx, 10_000, summarize); changed || err != nil || !reflect.DeepEqual(conv.Messages, before) {
			t.Error("conversation within budget was changed")
		}
	})

	t.Run("condenses older turns below the summary threshold", func(t *testing.T) {
		conv := turnsConversation(4, 40_000) // ~10k tokens per turn; only the latest fits a 15k budget
		calls := 0
		counting := func(ctx context.Context, history []chat.Message) (string, error) {
			calls++
			return summarize(ctx, history)
		}
		if changed, err := conv.FitContext(ctx, 15_000, counting); !changed || err != nil {
			t.Fatalf("over-budget conversation was not condensed: (%v, %v)", changed, err)
		}
		if calls != 1 {
			t.Errorf("summarizer called %d times, want once: the turns to keep are chosen up front", calls)
		}
		if got := conv.EstimateTokens(); got > 15_000 {
			t.Errorf("estimate %d still over budget", got)
		}
		n := len(conv.Messages)
		if conv.Messages[n-2].Content != "request 3" || len(conv.Messages[n-1].Content) != 40_000 {
			t.Error("latest turn was not kept in full")
		}
		if !strings.HasPrefix(conv.Messages[1].Content, session.SummaryPrefix) {
			t.Errorf("older turns not summarized: %q", conv.Messages[1].Content[:40])
		}
	})

	t.Run("prunes tool output within one turn", func(t *testing.T) {
		conv := &session.Conversation{Messages: []chat.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "go"}}}
		bulky := strings.Repeat("a line of tool output\n", 400)
		for i := range 5 {
			id := fmt.Sprintf("call_%d", i)
			conv.Messages = append(conv.Messages,
				chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id, Function: chat.ToolCallFunction{Name: "run_command"}}}},
				chat.Message{Role: "tool", ToolCallID: id, Content: bulky})
		}
		if changed, err := conv.FitContext(ctx, 3000, summarize); !changed || err != nil {
			t.Fatalf("over-budget turn was not condensed: (%v, %v)", changed, err)
		}
		last := conv.Messages[len(conv.Messages)-1]
		if last.Content != bulky {
			t.Error("latest tool output was pruned")
		}
		if conv.Messages[3].Content == bulky {
			t.Error("earlier tool output was kept in full")
		}
	})

	t.Run("does not re-wrap a summary it cannot shrink", func(t *testing.T) {
		conv := turnsConversation(3, 1000)
		conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: strings.Repeat("huge paste ", 10_000)})
		conv.FitContext(ctx, 100, summarize)
		after := slices.Clone(conv.Messages)
		if changed, err := conv.FitContext(ctx, 100, summarize); changed || err != nil || !reflect.DeepEqual(conv.Messages, after) {
			t.Error("a second pass changed a conversation that cannot shrink further")
		}
	})

	t.Run("a failed summary still prunes tool output and reports the error", func(t *testing.T) {
		conv := turnsConversation(4, 40_000)
		bulky := strings.Repeat("a line of tool output\n", 400)
		for i := range 3 {
			id := fmt.Sprintf("call_%d", i)
			conv.Messages = append(conv.Messages,
				chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: id, Function: chat.ToolCallFunction{Name: "run_command"}}}},
				chat.Message{Role: "tool", ToolCallID: id, Content: bulky})
		}
		failing := func(context.Context, []chat.Message) (string, error) { return "", fmt.Errorf("provider down") }
		changed, err := conv.FitContext(ctx, 15_000, failing)
		if err == nil || !changed {
			t.Fatalf("FitContext = (%v, %v), want pruning done and the summary error", changed, err)
		}
		if conv.Messages[1].Content != "request 0" || conv.Messages[len(conv.Messages)-1].Content != bulky {
			t.Error("turns must stay as they were and the latest tool output must be kept")
		}
		if conv.Messages[len(conv.Messages)-3].Content == bulky {
			t.Error("earlier tool output was not pruned")
		}
	})
}
