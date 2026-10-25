package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/terminal/theme"
	"hmj-agent/v2/internal/tools"
)

// TestBlocksHaveOneBlankLineBetweenThem runs a turn of text ending in line breaks, a tool call,
// then reasoning and the answer, and checks that each block has exactly one blank line before
// it: the text's own trailing breaks add none before the tool call, and the reasoning's line
// after the tool's result gets one, as text there would. The answer's last line is followed by
// the footer directly.
func TestBlocksHaveOneBlankLineBetweenThem(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data) }
		if requests.Add(1) == 1 {
			send("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Checking the file.\n\n"}`)
			send("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"slow_tool"}}`)
			send("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`)
		} else {
			send("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"weighing it"}`)
			send("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Done.\n"}`)
		}
		send("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)
	}))
	defer server.Close()

	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
	}}
	runner := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{fakeTool{}})
	conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}}
	out := theme.StripANSI(captureOutput(t, func() {
		if err := runner.Run(context.Background(), conv, "question"); err != nil {
			t.Errorf("Run: %v", err)
		}
	}))

	if strings.Contains(out, "\n\n\n") {
		t.Errorf("two blank lines in a row:\n%s", out)
	}
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^Checking the file\.\n\n(\S+ +)?slow_tool`),
		regexp.MustCompile(`(?m)^  \(\d+ms\)\n\n▸ Thought for 1s\n\nDone\.\n\[tokens:`),
	} {
		if !want.MatchString(out) {
			t.Errorf("output does not match %s:\n%s", want, out)
		}
	}
}

// TestTurnStartsWithOneBlankLine checks that whatever a turn prints first, a tool call or text,
// comes after exactly one blank line below the prompt: a tool call brings its own, which a blank
// line printed at the start of the turn used to double.
func TestTurnStartsWithOneBlankLine(t *testing.T) {
	for _, first := range []string{"tool", "text"} {
		t.Run(first, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(event, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data) }
				if requests.Add(1) == 1 && first == "tool" {
					send("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"slow_tool"}}`)
					send("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`)
				} else {
					send("response.output_text.delta", `{"type":"response.output_text.delta","delta":"Done."}`)
				}
				send("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)
			}))
			defer server.Close()

			cfg := config.Config{Providers: map[string]config.Provider{
				"p": {BaseURL: server.URL, API: "responses", Models: []config.Model{{ID: "m"}}},
			}}
			runner := NewRunnerWithTools(chat.NewClient(cfg), cfg, []tools.Tool{fakeTool{}})
			conv := &session.Conversation{Model: config.ModelRef{Provider: "p", Model: "m"}}
			out := theme.StripANSI(captureOutput(t, func() {
				if err := runner.Run(context.Background(), conv, "question"); err != nil {
					t.Errorf("Run: %v", err)
				}
			}))
			if !strings.HasPrefix(out, "\n") || strings.HasPrefix(out, "\n\n") || strings.Contains(out, "\n\n\n") {
				t.Errorf("turn output should start with exactly one blank line:\n%q", out)
			}
		})
	}
}
