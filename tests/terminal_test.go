package tests

import (
	"bufio"
	"strings"
	"testing"

	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
)

// The slash palette lists exactly the supported commands. /compact was removed: compaction
// runs automatically between turns.
func TestDefaultSlashCommands(t *testing.T) {
	var ids []string
	for _, cmd := range terminal.DefaultSlashCommands() {
		ids = append(ids, cmd.ID)
		if !strings.HasPrefix(cmd.Text, cmd.ID+" ") {
			t.Errorf("display text %q does not start with its command %s", cmd.Text, cmd.ID)
		}
	}
	want := []string{"/new", "/resume", "/models", "/health", "/history", "/shortcuts"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("slash commands = %v, want %v", ids, want)
	}
}

func TestComprehensiveTerminalComponents(t *testing.T) {
	// 1. Table rendering standard vs accessibility mode
	lines := []string{
		"| Name | Age | Role |",
		"| :--- | :--: | ---: |",
		"| Alice | 30 | Engineer |",
		"| Bob | 25 | Designer |",
	}
	tbl, ok := terminal.ParseMarkdownTable(lines)
	if !ok || tbl == nil {
		t.Fatal("expected table to parse successfully")
	}

	theme.SetAccessibility(false)
	rendered := tbl.Render()
	if !strings.Contains(rendered, "Alice") {
		t.Fatalf("expected Alice in rendered table, got:\n%s", rendered)
	}

	theme.SetAccessibility(true)
	defer theme.SetAccessibility(false)
	accessibleRendered := tbl.Render()
	if !strings.Contains(accessibleRendered, "+") || !strings.Contains(accessibleRendered, "|") {
		t.Fatalf("expected ASCII borders in accessible table, got:\n%s", accessibleRendered)
	}

	// 2. PromptEditor operations
	pe := terminal.NewPromptEditor("init")
	pe.SetWidth(80)
	if pe.Text() != "init" {
		t.Fatalf("expected 'init', got %s", pe.Text())
	}
	pe.Insert('!')
	pe.MoveHome()
	pe.InsertRunes([]rune("start-"))
	pe.MoveEnd()
	pe.Backspace()
	pe.MoveLeft()
	pe.Delete()
	pe.MoveRight()
	pe.MoveUp()
	pe.MoveDown()
	_ = pe.Cursor()
	_ = pe.TerminalWidth()
	linesOut, _, _ := pe.Layout()
	if len(linesOut) == 0 {
		t.Fatal("expected layout lines")
	}

	// 3. Prompt history
	terminal.SetPromptHistory([]string{"h1", "h2"})
	terminal.AddPromptHistory("h3")
	history := terminal.GetPromptHistory()
	if len(history) != 3 {
		t.Fatalf("expected 3 items in history, got %d", len(history))
	}

	// 4. Input cleaning
	clean := terminal.CleanInput("hello\r\n\tworld\x00")
	if !strings.Contains(clean, "hello") {
		t.Fatalf("unexpected clean input: %s", clean)
	}

	// 5. UnquoteDragPath
	testCases := []struct {
		input    string
		expected string
	}{
		{"'/home/user/my file.txt'", "/home/user/my file.txt"},
		{"\"/home/user/my file.txt\"", "/home/user/my file.txt"},
		{"/home/user/my\\ file.txt", "/home/user/my file.txt"},
		{"file:///home/user/my%20file.txt", "/home/user/my file.txt"},
		{"file://localhost/home/user/test.go", "/home/user/test.go"},
		{"plain_path.go", "plain_path.go"},
	}
	for _, tc := range testCases {
		if got := terminal.UnquoteDragPath(tc.input); got != tc.expected {
			t.Errorf("UnquoteDragPath(%q) = %q; expected %q", tc.input, got, tc.expected)
		}
	}

	// 6. FilterItems
	items := []terminal.SelectItem{
		{ID: "m1", Text: "GPT-4o"},
		{ID: "m2", Text: "Claude Sonnet"},
		{ID: "m3", Text: "Gemini Pro"},
	}
	all := terminal.FilterItems(items, "")
	if len(all) != 3 {
		t.Errorf("expected 3 items for empty query, got %d", len(all))
	}
	filtered := terminal.FilterItems(items, "claude")
	if len(filtered) != 1 || filtered[0].ID != "m2" {
		t.Errorf("expected Claude Sonnet, got: %+v", filtered)
	}
	filteredByID := terminal.FilterItems(items, "m3")
	if len(filteredByID) != 1 || filteredByID[0].ID != "m3" {
		t.Errorf("expected Gemini Pro, got: %+v", filteredByID)
	}
}

func TestBracketedPasteTogglesAreSafe(t *testing.T) {
	terminal.DisableBracketedPaste()
	terminal.EnableBracketedPaste()
	terminal.DisableBracketedPaste()
}

func TestUnquoteDragPathEdgeCases(t *testing.T) {
	cases := map[string]string{
		"plain.txt":      "plain.txt",
		`"/quoted path"`: "/quoted path",
		"":               "",
		`"unterminated`:  `"unterminated`,
		"  /spaced  ":    "/spaced",
	}
	for in, want := range cases {
		if got := terminal.UnquoteDragPath(in); got != want {
			t.Errorf("UnquoteDragPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPromptHistoryRoundTrip(t *testing.T) {
	terminal.SetPromptHistory(nil)
	terminal.AddPromptHistory("first")
	terminal.AddPromptHistory("second")
	got := terminal.GetPromptHistory()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("unexpected history: %v", got)
	}
	terminal.SetPromptHistory(nil)
	if got := terminal.GetPromptHistory(); len(got) != 0 {
		t.Errorf("expected cleared history, got %v", got)
	}
}

func TestFilterItems(t *testing.T) {
	items := []terminal.SelectItem{
		{ID: "new", Text: "New conversation"},
		{ID: "resume", Text: "Resume conversation"},
		{ID: "clear", Text: "Clear screen"},
	}
	if got := terminal.FilterItems(items, "conv"); len(got) != 2 {
		t.Errorf("expected 2 matches for 'conv', got %d", len(got))
	}
	if got := terminal.FilterItems(items, "clear"); len(got) != 1 || got[0].ID != "clear" {
		t.Errorf("expected exact 'clear' match, got %v", got)
	}
	if got := terminal.FilterItems(items, "zzz"); len(got) != 0 {
		t.Errorf("expected no matches, got %d", len(got))
	}
	if got := terminal.FilterItems(nil, "x"); got != nil {
		t.Errorf("nil items should return nil, got %v", got)
	}
}

func TestUnquoteDragPathPercentEncoding(t *testing.T) {
	cases := map[string]string{
		"file:///home/u/my%20file.txt":  "/home/u/my file.txt",
		"file:///home/u/100%25done.txt": "/home/u/100%done.txt",
		"file://localhost/x/y%2Fz":      "/x/y/z",
		"file:///bad%ZZsequence":        "/bad%ZZsequence", // invalid hex preserved
		"file:///truncated%2":           "/truncated%2",    // truncated escape preserved
		"file:///home/u/caf%C3%A9.txt":  "/home/u/caf\u00e9.txt",
	}
	for in, want := range cases {
		if got := terminal.UnquoteDragPath(in); got != want {
			t.Errorf("UnquoteDragPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanInput(t *testing.T) {
	raw := "\x1b[200~Create a scratch file\nwith \"mango\"\x1b[201~"
	cleaned := terminal.CleanInput(raw)
	if strings.Contains(cleaned, "[200~") || strings.Contains(cleaned, "[201~") {
		t.Fatalf("escape codes were not stripped: %s", cleaned)
	}
	if cleaned != "Create a scratch file\nwith \"mango\"" {
		t.Fatalf("unexpected cleaned output: %q", cleaned)
	}

	raw2 := "^[[200~Create file^[[201~"
	cleaned2 := terminal.CleanInput(raw2)
	if cleaned2 != "Create file" {
		t.Fatalf("unexpected caret cleaned output: %q", cleaned2)
	}
}

func TestReadSeamlessInputMultiLinePaste(t *testing.T) {
	paste := `create a scratch file called scratch.txt containing "apple banana orange", then edit it to replace "banana" with
  "mango"`

	reader := bufio.NewReader(strings.NewReader(paste + "\n"))
	res, _, err := terminal.ReadSeamlessInput(reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(res, "scratch.txt") {
		t.Fatalf("expected Line 1 content 'scratch.txt' in output, got:\n%s", res)
	}
	if !strings.Contains(res, `"mango"`) {
		t.Fatalf("expected Line 2 content '\"mango\"' in output, got:\n%s", res)
	}
}

func TestTerminalRestore(t *testing.T) {
	// Calling SaveOrigTermios and Restore repeatedly must be safe and idempotent
	terminal.SaveOrigTermios()
	terminal.Restore()
	terminal.Restore()
}

func TestKeyboardShortcuts(t *testing.T) {
	cases := []struct {
		name           string
		input          string
		expectedAction terminal.ShortcutAction
	}{
		{"Ctrl+N", "\x0e\n", terminal.ActionNew},
		{"Ctrl+R", "\x12\n", terminal.ActionResume},
		{"Ctrl+O", "\x0f\n", terminal.ActionModels},
		{"Ctrl+L is unbound", "\x0c\n", terminal.ActionNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tc.input))
			_, action, err := terminal.ReadSeamlessInput(reader)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if action != tc.expectedAction {
				t.Fatalf("expected %v, got %v", tc.expectedAction, action)
			}
		})
	}
}

func TestInteractiveSelect(t *testing.T) {
	// Empty items
	item, ok, err := terminal.InteractiveSelect("select", nil, 0, nil, nil)
	if err != nil || ok || item.ID != "" {
		t.Fatalf("expected empty SelectItem for empty items, got (%+v, %v, %v)", item, ok, err)
	}

	items := []terminal.SelectItem{
		{ID: "1", Text: "model-alpha", IsActive: false},
		{ID: "2", Text: "model-beta", IsActive: true},
		{ID: "3", Text: "model-gamma", IsActive: false},
	}

	// Selection via reader (number "2")
	reader := bufio.NewReader(strings.NewReader("2\n"))
	item, ok, err = terminal.InteractiveSelect("models", items, 0, reader, nil)
	if err != nil || !ok || item.ID != "2" {
		t.Fatalf("expected item ID 2, got (%+v, %v, %v)", item, ok, err)
	}

	// Search filter match via reader ("gamma")
	reader = bufio.NewReader(strings.NewReader("gamma\n"))
	item, ok, err = terminal.InteractiveSelect("models", items, 0, reader, nil)
	if err != nil || !ok || item.ID != "3" {
		t.Fatalf("expected item ID 3 for gamma, got (%+v, %v, %v)", item, ok, err)
	}

	// Cancellation via empty Enter
	reader = bufio.NewReader(strings.NewReader("\n"))
	item, ok, err = terminal.InteractiveSelect("models", items, 0, reader, nil)
	if err != nil || ok || item.ID != "" {
		t.Fatalf("expected empty SelectItem on cancel, got (%+v, %v, %v)", item, ok, err)
	}

	// End of input, unmatched text and a missing reader must cancel rather than
	// confirm the default item (defaultIdx 1 = model-beta).
	for name, reader := range map[string]*bufio.Reader{
		"eof":       bufio.NewReader(strings.NewReader("")),
		"no match":  bufio.NewReader(strings.NewReader("zeta\n")),
		"no reader": nil,
	} {
		item, ok, err = terminal.InteractiveSelect("models", items, 1, reader, nil)
		if err != nil || ok || item.ID != "" {
			t.Fatalf("%s: expected cancel, got (%+v, %v, %v)", name, item, ok, err)
		}
	}
}

func TestPromptEditorNavigationAndClear(t *testing.T) {
	p := "Line 1: first paragraph\nLine 2: second paragraph\nLine 3: third paragraph"
	ed := terminal.NewPromptEditor(p)

	if ed.Text() != p {
		t.Fatalf("expected %q, got %q", p, ed.Text())
	}

	lines, curLine, _ := ed.Layout()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if curLine != 2 {
		t.Fatalf("expected cursor on line 2 (0-indexed), got %d", curLine)
	}

	// Move Up twice -> should land on Line 1 (curLine = 0)
	ed.MoveUp()
	_, curLine, _ = ed.Layout()
	if curLine != 1 {
		t.Fatalf("expected cursor on line 1 after MoveUp, got %d", curLine)
	}

	ed.MoveUp()
	_, curLine, _ = ed.Layout()
	if curLine != 0 {
		t.Fatalf("expected cursor on line 0 after second MoveUp, got %d", curLine)
	}

	// Move Down once -> should land on Line 2 (curLine = 1)
	ed.MoveDown()
	_, curLine, _ = ed.Layout()
	if curLine != 1 {
		t.Fatalf("expected cursor on line 1 after MoveDown, got %d", curLine)
	}

	// MoveHome -> should land at start of Line 2
	ed.MoveHome()
	_, curLine, curCol := ed.Layout()
	if curLine != 1 || curCol != 0 {
		t.Fatalf("expected cursor at line 1 col 0, got line %d col %d", curLine, curCol)
	}

	// Insert in middle
	ed.Insert('X')
	if !strings.Contains(ed.Text(), "\nXLine 2:") {
		t.Fatalf("expected inserted X, got %q", ed.Text())
	}

	// Clear() should wipe all text
	ed.Clear()
	if ed.Text() != "" {
		t.Fatalf("expected empty after Clear, got %q", ed.Text())
	}

	// renderedLines must be 0 after Clear
	lines2, curLine2, _ := ed.Layout()
	if len(lines2) != 1 || curLine2 != 0 {
		t.Fatalf("expected 1 line at cursor 0 after Clear, got %d lines curLine %d", len(lines2), curLine2)
	}
}

// TestPromptEditorClearMultiLine verifies that:
// 1. After inserting multi-line text, renderedLines equals len(lines)-1
// 2. After navigating away from the last line, renderedLines still tracks total
// 3. Clear() resets all state (runes, cursor, renderedLine, renderedLines)
// This is a regression test for the bug where Clear()/Esc only erased one
// line when the cursor was on line 0 of a multi-line paste.
func TestPromptEditorClearMultiLine(t *testing.T) {
	ed := terminal.NewPromptEditor("")

	// Insert 4 lines
	fourLines := "alpha\nbeta\ngamma\ndelta"
	ed.InsertRunes([]rune(fourLines))

	// Verify layout: 4 lines, cursor on last line (line 3)
	lines, curLine, _ := ed.Layout()
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
	if curLine != 3 {
		t.Fatalf("expected cursor on line 3 after insert, got %d", curLine)
	}

	// RenderedLines (via Layout) should reflect total — 4 lines means index 3
	// Move cursor to line 0 (simulate user pressing Up arrow 3 times)
	ed.MoveUp()
	ed.MoveUp()
	ed.MoveUp()
	_, curLine, _ = ed.Layout()
	if curLine != 0 {
		t.Fatalf("expected cursor on line 0 after MoveUp x3, got %d", curLine)
	}

	// Text must still be intact — all 4 lines present
	if ed.Text() != fourLines {
		t.Fatalf("text changed after navigation: %q", ed.Text())
	}

	// Clear() must wipe everything regardless of cursor position
	ed.Clear()
	if ed.Text() != "" {
		t.Fatalf("expected empty text after Clear, got %q", ed.Text())
	}
	if ed.Cursor() != 0 {
		t.Fatalf("expected cursor 0 after Clear, got %d", ed.Cursor())
	}

	// After Clear, layout must be 1 line at position 0
	lines, curLine, _ = ed.Layout()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line after Clear, got %d", len(lines))
	}
	if curLine != 0 {
		t.Fatalf("expected cursor line 0 after Clear, got %d", curLine)
	}

	// Re-insert after clear must work cleanly
	ed.InsertRunes([]rune("new text"))
	if ed.Text() != "new text" {
		t.Fatalf("expected 'new text' after re-insert, got %q", ed.Text())
	}
}

// TestPromptEditorSoftWrapVisualLayout is a regression test for line duplication
// when prompt input crosses terminal width boundary and soft-wraps.
func TestPromptEditorSoftWrapVisualLayout(t *testing.T) {
	ed := terminal.NewPromptEditor("")
	const termWidth = 40
	ed.SetWidth(termWidth)

	// 1. Single long line without \n: 98 runes + prompt "> " (2 runes) = 100 characters total.
	// At termWidth 40:
	// Row 0: 40 chars
	// Row 1: 40 chars
	// Row 2: 20 chars
	// Total visual rows must be 3, cursor must be on visual row 2, visual col 20.
	longText := strings.Repeat("a", 98)
	ed.InsertRunes([]rune(longText))

	vl := ed.VisualLayout(termWidth)
	if vl.TotalVisualRows != 3 {
		t.Fatalf("expected 3 visual rows for 100 chars at width 40, got %d", vl.TotalVisualRows)
	}
	if vl.CursorVisualRow != 2 {
		t.Fatalf("expected cursor at visual row 2, got %d", vl.CursorVisualRow)
	}
	if vl.CursorVisualCol != 20 {
		t.Fatalf("expected cursor at visual col 20, got %d", vl.CursorVisualCol)
	}

	// 2. Move cursor left across the soft-wrap boundary
	// Move left 25 times -> cursor position is now at char index 73 (total 75 chars with prompt)
	// 75 chars at width 40: Row 1, Column 35.
	for range 25 {
		ed.MoveLeft()
	}
	vl = ed.VisualLayout(termWidth)
	if vl.CursorVisualRow != 1 {
		t.Fatalf("expected cursor at visual row 1 after moving left 25 times, got %d", vl.CursorVisualRow)
	}
	if vl.CursorVisualCol != 35 {
		t.Fatalf("expected cursor at visual col 35, got %d", vl.CursorVisualCol)
	}

	// MoveHome -> should land on Row 0, Column 2 (right after prompt "> ")
	ed.MoveHome()
	vl = ed.VisualLayout(termWidth)
	if vl.CursorVisualRow != 0 {
		t.Fatalf("expected cursor at visual row 0 after MoveHome, got %d", vl.CursorVisualRow)
	}
	if vl.CursorVisualCol != 2 {
		t.Fatalf("expected cursor at visual col 2 after MoveHome, got %d", vl.CursorVisualCol)
	}

	// MoveEnd -> should land on Row 2, Column 20
	ed.MoveEnd()
	vl = ed.VisualLayout(termWidth)
	if vl.CursorVisualRow != 2 {
		t.Fatalf("expected cursor at visual row 2 after MoveEnd, got %d", vl.CursorVisualRow)
	}
	if vl.CursorVisualCol != 20 {
		t.Fatalf("expected cursor at visual col 20 after MoveEnd, got %d", vl.CursorVisualCol)
	}

	// 3. Clear() must reset everything cleanly
	ed.Clear()
	vl = ed.VisualLayout(termWidth)
	if vl.TotalVisualRows != 1 || vl.CursorVisualRow != 0 {
		t.Fatalf("expected 1 visual row at cursor 0 after Clear, got total %d row %d", vl.TotalVisualRows, vl.CursorVisualRow)
	}

	// 4. Multi-line text with soft-wrapping on individual lines
	// Line 1: 58 chars + prompt (2 chars) = 60 chars -> 2 visual rows (40 + 20)
	// Line 2: 78 chars + indent (2 chars) = 80 chars -> 2 visual rows (40 + 40)
	// Total visual rows: 4 rows.
	multiLine := strings.Repeat("x", 58) + "\n" + strings.Repeat("y", 78)
	ed.InsertRunes([]rune(multiLine))

	vl = ed.VisualLayout(termWidth)
	if vl.TotalVisualRows != 4 {
		t.Fatalf("expected 4 visual rows for multi-line wrapped text, got %d", vl.TotalVisualRows)
	}
	if vl.CursorVisualRow != 3 {
		t.Fatalf("expected cursor on visual row 3 (last row of Line 2), got %d", vl.CursorVisualRow)
	}
}

func TestPromptHistory(t *testing.T) {
	terminal.SetPromptHistory(nil)
	if h := terminal.GetPromptHistory(); len(h) != 0 {
		t.Fatalf("expected empty history, got %v", h)
	}

	terminal.AddPromptHistory("prompt 1")
	terminal.AddPromptHistory("prompt 2")
	terminal.AddPromptHistory("prompt 2") // duplicate consecutive should be ignored
	terminal.AddPromptHistory("  ")       // blank should be ignored
	terminal.AddPromptHistory("prompt 3")

	h := terminal.GetPromptHistory()
	if len(h) != 3 {
		t.Fatalf("expected 3 history items, got %d: %v", len(h), h)
	}
	if h[0] != "prompt 1" || h[1] != "prompt 2" || h[2] != "prompt 3" {
		t.Fatalf("unexpected history items: %v", h)
	}

	terminal.SetPromptHistory([]string{"alpha", "beta", "gamma"})
	h = terminal.GetPromptHistory()
	if len(h) != 3 || h[0] != "alpha" || h[1] != "beta" || h[2] != "gamma" {
		t.Fatalf("unexpected set history: %v", h)
	}
}

func TestPromptEditorHistoryNavigation(t *testing.T) {
	terminal.SetPromptHistory([]string{"first command", "second command", "third command"})

	ed := terminal.NewPromptEditor("")
	ed.InsertRunes([]rune("current draft"))

	// MoveUp once -> should recall "third command"
	ed.MoveUp()
	if ed.Text() != "third command" {
		t.Fatalf("expected 'third command', got %q", ed.Text())
	}

	// MoveUp again -> should recall "second command"
	ed.MoveUp()
	if ed.Text() != "second command" {
		t.Fatalf("expected 'second command', got %q", ed.Text())
	}

	// MoveUp again -> should recall "first command"
	ed.MoveUp()
	if ed.Text() != "first command" {
		t.Fatalf("expected 'first command', got %q", ed.Text())
	}

	// MoveUp at beginning -> should stay at "first command"
	ed.MoveUp()
	if ed.Text() != "first command" {
		t.Fatalf("expected 'first command', got %q", ed.Text())
	}

	// MoveDown -> should go forward to "second command"
	ed.MoveDown()
	if ed.Text() != "second command" {
		t.Fatalf("expected 'second command', got %q", ed.Text())
	}

	// MoveDown -> should go forward to "third command"
	ed.MoveDown()
	if ed.Text() != "third command" {
		t.Fatalf("expected 'third command', got %q", ed.Text())
	}

	// MoveDown -> should restore "current draft"
	ed.MoveDown()
	if ed.Text() != "current draft" {
		t.Fatalf("expected 'current draft', got %q", ed.Text())
	}
}

func TestActionHistoryShortcut(t *testing.T) {
	// Ctrl+Y (ASCII 0x19) triggers ActionHistory
	input := "\x19"
	reader := bufio.NewReader(strings.NewReader(input))
	_, action, err := terminal.ReadSeamlessInput(reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != terminal.ActionHistory {
		t.Fatalf("expected ActionHistory, got %v", action)
	}
}

func TestClipboard(t *testing.T) {
	// CopyToClipboard must never report a false success: it either lands the
	// text through a real channel (OSC 52 or a native utility) or returns an
	// error explaining exactly why the clipboard was unavailable.
	err := terminal.CopyToClipboard("hello world")
	if err != nil && !strings.Contains(err.Error(), "clipboard") {
		t.Fatalf("unexpected CopyToClipboard error: %v", err)
	}
}

func TestPromptEditorCursorAndEdits(t *testing.T) {
	ed := terminal.NewPromptEditor("abc")
	ed.SetWidth(80)
	if ed.TerminalWidth() != 80 {
		t.Fatalf("expected 80, got %d", ed.TerminalWidth())
	}
	if ed.Cursor() != 3 {
		t.Fatalf("expected cursor 3, got %d", ed.Cursor())
	}

	// MoveLeft and MoveRight
	ed.MoveLeft()
	if ed.Cursor() != 2 {
		t.Fatalf("expected cursor 2, got %d", ed.Cursor())
	}
	ed.MoveRight()
	if ed.Cursor() != 3 {
		t.Fatalf("expected cursor 3, got %d", ed.Cursor())
	}
	ed.MoveRight() // no-op at end
	if ed.Cursor() != 3 {
		t.Fatalf("expected cursor still 3, got %d", ed.Cursor())
	}

	// MoveHome and MoveEnd
	ed.MoveHome()
	if ed.Cursor() != 0 {
		t.Fatalf("expected cursor 0, got %d", ed.Cursor())
	}
	ed.MoveLeft() // no-op at start
	if ed.Cursor() != 0 {
		t.Fatalf("expected cursor still 0, got %d", ed.Cursor())
	}
	ed.Delete() // deletes 'a', text becomes "bc"
	if ed.Text() != "bc" {
		t.Fatalf("expected 'bc', got %q", ed.Text())
	}

	ed.MoveEnd()
	if ed.Cursor() != 2 {
		t.Fatalf("expected cursor 2, got %d", ed.Cursor())
	}
	ed.Delete() // no-op at end
	if ed.Text() != "bc" {
		t.Fatalf("expected 'bc', got %q", ed.Text())
	}
	ed.Backspace() // deletes 'c', text becomes "b"
	if ed.Text() != "b" {
		t.Fatalf("expected 'b', got %q", ed.Text())
	}

	// Insert runes empty slice
	ed.InsertRunes(nil)
	if ed.Text() != "b" {
		t.Fatalf("expected 'b', got %q", ed.Text())
	}

	// Backspace on empty
	ed.Backspace()
	ed.Backspace() // no-op on empty
	if ed.Text() != "" {
		t.Fatalf("expected empty, got %q", ed.Text())
	}
}
