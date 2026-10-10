package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
)

// TestAccessibleThoughtsEndWithLabel checks that in accessibility mode the reasoning block is closed by
// an [END THOUGHTS] line before the answer text begins, and that the normal view does not print it.
func TestAccessibleThoughtsEndWithLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","delta":"weighing options"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"The answer"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed"}}

`)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	run := func(accessible bool) string {
		theme.SetAccessibility(accessible)
		t.Cleanup(func() { theme.SetAccessibility(false) })
		runner := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
		conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}}
		return captureOutput(t, func() {
			if err := runner.Run(context.Background(), conv, "question"); err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}

	out := run(true)
	end := strings.Index(out, endThoughtsLabel)
	answer := strings.Index(out, "The answer")
	if end < 0 || answer < 0 || end > answer {
		t.Fatalf("accessible output = %q, want %s before the answer", out, endThoughtsLabel)
	}
	if !strings.Contains(out, "weighing options") {
		t.Errorf("thoughts were not streamed: %q", out)
	}

	if out := run(false); strings.Contains(out, endThoughtsLabel) {
		t.Errorf("normal output must not print %s: %q", endThoughtsLabel, out)
	}
}
