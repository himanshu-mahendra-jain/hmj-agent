package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// topics returns the names "topic 01" to "topic NN".
func topics(n int) []string {
	var names []string
	for i := 1; i <= n; i++ {
		names = append(names, fmt.Sprintf("topic %02d", i))
	}
	return names
}

// writeConversations writes a session file holding one empty conversation per name, the
// last one active.
func writeConversations(t *testing.T, e *env, names []string) {
	t.Helper()
	n := len(names)
	type conv struct {
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Model     map[string]string `json:"model"`
		Messages  []any             `json:"messages"`
		CreatedAt time.Time         `json:"createdAt"`
		UpdatedAt time.Time         `json:"updatedAt"`
	}
	model := map[string]string{"provider": "mock", "model": "model-a"}
	convs := map[string]conv{}
	var order []string
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		id := fmt.Sprint(i)
		convs[id] = conv{ID: id, Name: names[i-1], Model: model, Messages: []any{},
			CreatedAt: base.Add(time.Duration(i) * time.Minute), UpdatedAt: base.Add(time.Duration(i) * time.Minute)}
		order = append(order, id)
	}
	data, err := json.Marshal(map[string]any{
		"activeId": fmt.Sprint(n), "conversations": convs, "order": order, "counter": n, "defaultModel": model,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Sessions, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// menuItemRows counts the rows of the open menu that show a numbered item.
func menuItemRows(v *vtScreen) int {
	n := 0
	for _, l := range v.Lines() {
		if strings.Contains(l, "topic ") && strings.Contains(l, ". [") {
			n++
		}
	}
	return n
}

// TestResumeSelectorScrollsThroughManyConversations opens the resume selector over twelve
// conversations: it shows a window of seven with scroll badges, wraps at both ends, keeps
// the highlight in range when the last item is deleted, and filters and unfilters.
func TestResumeSelectorScrollsThroughManyConversations(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.WriteConfig(p.config("model-a"))
	writeConversations(t, e, topics(12))
	s := e.Start()
	s.ExpectPrompt()

	s.Type("\x12") // Ctrl+R opens on the active conversation, the last one
	v := s.ExpectScreen("the last conversation highlighted, five more above", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 12") && v.RowOf("12/12 matches [↑ 5 more]") >= 0
	})
	if n := menuItemRows(v); n != 7 {
		t.Errorf("%d item rows on screen, want a window of 7\n%s", n, v.Dump())
	}
	s.Type("\x1b[B") // Down from the last item wraps to the first
	v = s.ExpectScreen("the first conversation highlighted, five more below", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 01") && v.RowOf("1/12 matches [↓ 5 more]") >= 0
	})
	if n := menuItemRows(v); n != 7 {
		t.Errorf("%d item rows on screen after wrapping, want 7\n%s", n, v.Dump())
	}
	s.Type("\x1b[A") // Up from the first wraps to the last
	s.ExpectScreen("the last conversation highlighted again", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 12") && v.RowOf("12/12 matches [↑ 5 more]") >= 0
	})
	s.Type(strings.Repeat("\x1b[A", 8)) // eight up: item 4, the window scrolled to start there
	s.ExpectScreen("item 4 highlighted at the top of the window", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 04") && v.RowOf("4/12 matches [↑ 3 more, ↓ 2 more]") >= 0
	})

	// Delete the last conversation while it is highlighted: the highlight moves to the new last.
	s.Type("\x1b[A\x1b[A\x1b[A\x1b[A") // 4 → 3 → 2 → 1, then wraps to 12
	s.ExpectScreen("the last conversation highlighted", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 12")
	})
	s.Type("\x04")
	s.Expect("[y/N]")
	s.Type("y")
	s.ExpectScreen("eleven left, the new last one highlighted", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 11") && v.RowOf("deleted conversation") >= 0 && v.RowOf("topic 12") < 0
	})
	s.Type("\x1b[B")
	s.ExpectScreen("the footer counts eleven", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 01") && v.RowOf("1/11 matches") >= 0
	})

	// Filter, then clear the filter with Ctrl+U.
	s.TypeSlowly("topic 1")
	s.ExpectScreen("two matches", func(v *vtScreen) bool { return v.RowOf("1/2 matches") >= 0 && menuItemRows(v) == 2 })
	s.Type("\x15")
	s.ExpectScreen("all eleven again", func(v *vtScreen) bool { return v.RowOf("1/11 matches [↓ 4 more]") >= 0 })

	// Rename cancelled with Esc keeps the name and the menu.
	s.Type("\x05")
	s.Expect("rename to:")
	s.TypeSlowly("other")
	s.PressEsc()
	s.Expect("rename cancelled")

	s.Type("\x1b[A\r") // Up wraps to item 11; Enter resumes it
	s.Expect("resumed conversation")
	s.Expect("topic 11")
	s.ExpectPrompt()
	s.Quit()
}

// TestResumeSelectorInShortTerminal opens the resume selector in a terminal with fewer
// rows than the menu is tall and moves through it. Each redraw must replace the last one:
// the title and the footer appear once.
func TestResumeSelectorInShortTerminal(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Rows, e.Cols = 8, 80
	e.WriteConfig(p.config("model-a"))
	writeConversations(t, e, topics(12))
	s := e.Start()
	s.ExpectPrompt()
	s.Type("\x12")
	s.Expect("matches")
	for range 3 {
		s.Type("\x1b[B")
	}
	v := s.ExpectScreen("item 3 highlighted", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "topic 03")
	})
	if n := strings.Count(v.Text(), "matches"); n != 1 {
		t.Errorf("the footer is on screen %d times, want once\n%s", n, v.Dump())
	}
	s.PressEsc()
	s.ExpectPrompt()
	s.Type("hi\r")
	s.Expect("ok")
	s.ExpectPrompt()
	s.Quit()
}

// TestHaltWhileCompacting presses Esc, and in a second run Ctrl+C, while the next prompt waits
// for a compaction that has not ended. The halt stops the compaction without an error, the prompt
// is sent with the whole history, and compaction runs again after that turn.
func TestHaltWhileCompacting(t *testing.T) {
	t.Parallel()
	for _, key := range []struct{ name, keys, notice string }{
		{"Esc", "\x1b", "[halted by user (Esc)]"},
		{"Ctrl+C", "\x03", "[halted by user (Ctrl+C)]"},
	} {
		t.Run(key.name, func(t *testing.T) {
			t.Parallel()
			p := newProvider(t)
			_, s := start(t, p)
			p.Queue(sayWithPrompt("answer 1", overLimit), sayWithPrompt("answer 2", overLimit), say("answer 3"))
			p.QueueCompact(hang(), compactedWindow("WINDOW of the questions"))
			s.Type("question 1\r")
			s.Expect("answer 1")
			s.ExpectPrompt()
			s.Type("question 2\r")
			s.Expect("compacting the conversation")
			s.Type(key.keys)
			s.Expect(key.notice)
			s.Expect("[compaction stopped")
			s.Expect("answer 2")
			s.ExpectPrompt()
			if strings.Contains(s.Screen(), "could not compact") {
				t.Errorf("a halted compaction was reported as a failure")
			}
			if turn := p.LastRequest(); !strings.Contains(turn, `"question 1"`) || !strings.Contains(turn, `"question 2"`) {
				t.Errorf("the turn after a halted compaction should carry the whole history:\n%s", turn)
			}

			s.Type("question 3\r")
			s.Expect("[compacted the conversation:")
			s.Expect("answer 3")
			s.ExpectPrompt()
			if turn := p.LastRequest(); !strings.Contains(turn, "WINDOW of the questions") || !strings.Contains(turn, `"question 3"`) || strings.Contains(turn, `"question 1"`) {
				t.Errorf("the turn after the compaction should carry the window and the new prompt:\n%s", turn)
			}
			s.Quit()
		})
	}
}

// TestEscDuringToolCallOutputKeepsPromptUsable halts a turn with Esc while a tool's output is
// still being produced, then checks that a typed prompt is not swallowed by the halt
// listener and the next turn answers the dangling tool call.
func TestEscDuringToolCallOutputKeepsPromptUsable(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(callTool("run_command", map[string]any{"command": "echo started; sleep 30"}))
	s.Type("slow tool\r")
	s.Expect("run_command")
	s.PressEsc()
	s.Expect("[halted by user (Esc)]")
	s.ExpectPrompt()
	s.TypeSlowly("after halt\r")
	s.Expect("ok")
	s.ExpectPrompt()
	if req := p.LastRequest(); !strings.Contains(req, "call_run_command") || !strings.Contains(req, `"after halt"`) {
		t.Errorf("the next request should answer the halted call and carry the new prompt: %s", req)
	}
	s.Quit()
}

// TestResumeSelectorSanitizesConversationNames lists conversations whose names, edited
// into the session file or written by a model, hold a Tab, a newline, escape sequences and
// more text than fits. Each item stays on one row, the escapes do nothing, and the menu
// redraws in place.
func TestResumeSelectorSanitizesConversationNames(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e := newEnv(t)
	e.Cols = 60
	e.WriteConfig(p.config("model-a"))
	writeConversations(t, e, []string{
		"tabbed\tname\twith\tmany\ttabs\tin\tit\tand\tmore\ttabs",
		"two\nlines",
		"clear\x1b[2J\x1b[Hscreen\x1b]0;pwn5\x07",
		strings.Repeat("long name ", 12),
	})
	s := e.Start()
	s.ExpectPrompt()
	s.Type("\x12")
	s.Expect("matches")
	s.Type("\x1b[A\x1b[A")
	v := s.ExpectScreen("the second conversation highlighted", func(v *vtScreen) bool {
		return strings.Contains(highlightedRow(v), "two")
	})
	if v.RowOf("[model: mock/model-a]") < 0 {
		t.Errorf("a conversation name cleared the screen:\n%s", v.Dump())
	}
	for _, osc := range v.oscs {
		if strings.Contains(osc, "pwn5") {
			t.Errorf("a conversation name set the terminal title: %q", osc)
		}
	}
	if n := strings.Count(v.Text(), "select conversation"); n != 1 {
		t.Errorf("the menu title is on screen %d times, want once (a row wrapped)\n%s", n, v.Dump())
	}
	tabRow := v.RowOf("tabbed")
	if tabRow < 0 || v.RowOf("two") != tabRow+1 || v.RowOf("clear") != tabRow+2 || v.RowOf("long name") != tabRow+3 {
		t.Errorf("each conversation should take exactly one row\n%s", v.Dump())
	}
	if v.RowOf("lines") != v.RowOf("two") {
		t.Errorf("the newline in a name broke its row\n%s", v.Dump())
	}
	s.PressEsc()
	s.ExpectPrompt()
	s.Quit()
}
