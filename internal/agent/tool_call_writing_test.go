package agent

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/terminal"
	"hmj-agent/v2/internal/terminal/theme"
)

// TestStatusLineShowsTheToolCallBeingWritten checks that once the model's text has streamed, the
// status line names the tool call it is writing and the size of its arguments so far, and that
// the text's last line is ended first.
func TestStatusLineShowsTheToolCallBeingWritten(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event, data string, pause time.Duration) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			w.(http.Flusher).Flush()
			time.Sleep(pause) // long enough for the status line to be drawn
		}
		send("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Writing the file."}`, 0)
		send("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"write_file"}}`, 250*time.Millisecond)
		send("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":"}`, 250*time.Millisecond)
		send("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"a.go\"}"}`, 250*time.Millisecond)
		send("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`, 0)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
	conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}, Messages: []chat.Message{{Role: "user", Content: "write it"}}}
	var status bytes.Buffer
	spinner := terminal.NewSpinnerTo("thinking…", &status, true)

	out := captureOutput(t, func() {
		if _, _, err := r.streamStep(context.Background(), conv, spinner); err != nil {
			t.Errorf("streamStep: %v", err)
		}
	})
	if out != "\nWriting the file.\n" {
		t.Errorf("transcript = %q, want the text on its own line after a blank one", out)
	}
	drawn := theme.StripANSI(status.String())
	for _, want := range []string{"writing write_file (0s)", "writing write_file (0s) · 8 B", "writing write_file (0s) · 15 B"} {
		if !strings.Contains(drawn, want) {
			t.Errorf("status line never showed %q: %q", want, drawn)
		}
	}
}
