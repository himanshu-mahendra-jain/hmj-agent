package chat_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/tools"
)

// TestAnthropicToolFlags sends the agent's own tools to a mock Anthropic API and checks that none
// is strict, under which Claude must write arguments in schema order and lost grep's path and
// pattern, and that write_file and edit_file, alone, have their input streamed as it is written.
func TestAnthropicToolFlags(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"a": {BaseURL: server.URL, APIKey: "k", API: "anthropic", Models: []config.Model{{ID: "claude-opus-5-5"}}},
	}}
	var defs []chat.ToolDefinition
	for _, tl := range tools.DefaultTools() {
		defs = append(defs, tl.Definition())
	}
	if _, _, err := chat.NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "a", Model: "claude-opus-5-5"},
		[]chat.Message{{Role: "user", Content: "hi"}}, defs, func(string) {}, func(string) {}); err != nil {
		t.Fatal(err)
	}

	var req struct {
		Tools []struct {
			Name   string `json:"name"`
			Strict *bool  `json:"strict"`
			Eager  bool   `json:"eager_input_streaming"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != len(defs) {
		t.Fatalf("sent %d tools, want %d", len(req.Tools), len(defs))
	}
	for _, tl := range req.Tools {
		if tl.Strict != nil {
			t.Errorf("%s: strict is sent (%v)", tl.Name, *tl.Strict)
		}
		if want := tl.Name == "write_file" || tl.Name == "edit_file"; tl.Eager != want {
			t.Errorf("%s: eager_input_streaming %v, want %v", tl.Name, tl.Eager, want)
		}
	}
}
