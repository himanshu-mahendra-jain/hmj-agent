package agent

import (
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
)

// TestBeginTurnKeepsSystemPromptOfStartedConversation checks that a conversation which already has a
// user message keeps its system prompt, so the cached prefix stays valid, while a new conversation
// gets the current one.
func TestBeginTurnKeepsSystemPromptOfStartedConversation(t *testing.T) {
	r := NewRunnerWithTools(chat.NewClient(config.Config{}), config.Config{}, nil)

	started := &session.Conversation{Messages: []chat.Message{
		{Role: "system", Content: "ORIGINAL"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
	}}
	r.beginTurn(started, "second")
	if started.Messages[0].Content != "ORIGINAL" {
		t.Errorf("system prompt of a started conversation changed to %q", started.Messages[0].Content)
	}

	fresh := &session.Conversation{}
	r.beginTurn(fresh, "hello")
	if len(fresh.Messages) < 2 || fresh.Messages[0].Role != "system" || fresh.Messages[0].Content != session.CurrentSystemPrompt() {
		t.Errorf("new conversation messages = %+v, want the current system prompt first", fresh.Messages)
	}
}
