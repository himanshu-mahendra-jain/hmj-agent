package agent

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/logging"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
	"hmj-agent/internal/tools"
)

// HandleNew creates and switches to a new conversation and clears the screen. Earlier
// conversations are kept and stay available in /resume.
func HandleNew(sm *session.SessionManager) error {
	conv := sm.New("")
	if err := sm.Save(); err != nil {
		return err
	}
	// Clear-screen escapes only make sense on a real terminal; never emit
	// them into piped/captured output, and accessibility output is append-only.
	if theme.IsTerminal(os.Stdout.Fd()) && !theme.Accessibility() {
		fmt.Print("\033[H\033[2J\033[3J")
		PrintBanner()
	}
	fmt.Printf("%s %s %s\n", theme.OK("created conversation"), theme.Accent(conv.Name), theme.Muted("(id: "+conv.ID+")"))
	return nil
}

// HandleModels opens the interactive model selector.
func HandleModels(cfg config.Config, sm *session.SessionManager, reader *bufio.Reader) error {
	active := sm.Active()
	models := cfg.AllModels()
	if len(models) == 0 {
		fmt.Println("no models configured")
		return nil
	}

	defaultIdx := 0
	var items []terminal.SelectItem
	for i, m := range models {
		isActive := (m.Provider == active.Model.Provider && m.Model == active.Model.Model)
		if isActive {
			defaultIdx = i
		}
		text := fmt.Sprintf("%s/%s", m.Provider, m.Model)
		if p, modelObj, err := cfg.ResolveModel(m); err == nil {
			if modelObj.Name != "" {
				text = fmt.Sprintf("%s/%s (%s)", m.Provider, m.Model, modelObj.Name)
			}
			text += theme.ASCII(" · ") + "effort: " + chat.EffortLabel(p, modelObj)
		}
		items = append(items, terminal.SelectItem{
			ID:       fmt.Sprintf("%s/%s", m.Provider, m.Model),
			Text:     text,
			IsActive: isActive,
		})
	}

	chosenItem, confirmed, err := terminal.InteractiveSelect(
		"select model (type to filter, ↑/↓ move, Enter select, Esc cancel)",
		items, defaultIdx, reader, nil,
	)
	if err != nil {
		return err
	}
	if !confirmed || chosenItem.Text == "" {
		return nil
	}

	ref, err := cfg.FindModel(chosenItem.ID)
	if err != nil {
		ref, err = cfg.FindModel(chosenItem.Text)
		if err != nil {
			return err
		}
	}
	active.Model = ref
	sm.SetDefaultModel(ref)
	if err := sm.Save(); err != nil {
		return err
	}
	fmt.Printf("%s %s/%s\n", theme.OK("model:"), theme.Accent(ref.Provider), theme.Accent(ref.Model))
	return nil
}

// resumeItemLabel formats a conversation as its label in the resume selector, noting when
// another hmj has it open.
func resumeItemLabel(c *session.Conversation, openElsewhere bool) string {
	msgStr := "messages"
	if c.UserMessageCount() == 1 {
		msgStr = "message"
	}
	label := fmt.Sprintf("[%s] %s (%d user %s, model: %s/%s)", c.ID, c.Name, c.UserMessageCount(), msgStr, c.Model.Provider, c.Model.Model)
	if openElsewhere {
		label += " [open in another hmj]"
	}
	return label
}

// buildResumeItems converts the conversations into selector items and returns the
// index of the active conversation (0 when none is active).
func buildResumeItems(convs []*session.Conversation, sm *session.SessionManager) ([]terminal.SelectItem, int) {
	activeID := sm.ActiveID
	defaultIdx := 0
	var items []terminal.SelectItem
	for i, c := range convs {
		isActive := (c.ID == activeID)
		if isActive {
			defaultIdx = i
		}
		items = append(items, terminal.SelectItem{
			ID:       c.ID,
			Text:     resumeItemLabel(c, sm.OpenElsewhere(c.ID)),
			IsActive: isActive,
		})
	}
	return items, defaultIdx
}

// resumeCallbacks returns the rename and delete actions of the resume selector. Each
// successful action appends a status line to actionMsgs, printed when the selector closes.
func resumeCallbacks(sm *session.SessionManager, actionMsgs *[]string) *terminal.SelectCallbacks {
	return &terminal.SelectCallbacks{
		OnRename: func(item terminal.SelectItem, newName string) (string, error) {
			err := sm.Rename(item.ID, newName)
			if err != nil {
				return "", err
			}
			if err := sm.Save(); err != nil {
				return "", err
			}
			*actionMsgs = append(*actionMsgs, fmt.Sprintf("renamed conversation to: %s", newName))
			conv := sm.Conversations[item.ID]
			return resumeItemLabel(conv, false), nil
		},
		OnDelete: func(item terminal.SelectItem) error {
			convName := item.ID
			if c, ok := sm.Conversations[item.ID]; ok {
				convName = c.Name
			}
			err := sm.Delete(item.ID)
			if err != nil {
				return err
			}
			if err := sm.Save(); err != nil {
				return err
			}
			*actionMsgs = append(*actionMsgs, fmt.Sprintf("deleted conversation: %s", convName))
			return nil
		},
	}
}

// printResumedConversation prints the resume confirmation and, when the conversation
// has user messages, its history.
func printResumedConversation(conv *session.Conversation) {
	fmt.Printf("%s %s %s %s %s\n",
		theme.OK("resumed conversation"),
		theme.Accent(conv.Name),
		theme.Muted("(id: "+conv.ID+","),
		theme.Muted(fmt.Sprintf("%d user messages)", conv.UserMessageCount())),
		theme.Muted("model: "+conv.Model.Provider+"/"+conv.Model.Model),
	)
	if conv.UserMessageCount() > 0 {
		PrintConversationHistory(conv)
	}
}

// HandleResume opens the interactive conversation resume / manager selector.
func HandleResume(cfg config.Config, sm *session.SessionManager, reader *bufio.Reader) error {
	convs := sm.List()
	if len(convs) == 0 {
		fmt.Println("no conversations")
		return nil
	}

	items, defaultIdx := buildResumeItems(convs, sm)

	var actionMsgs []string
	cb := resumeCallbacks(sm, &actionMsgs)

	chosenItem, confirmed, err := terminal.InteractiveSelect(
		"select conversation (type to filter, ↑/↓ move, Enter select, Ctrl+E rename, Ctrl+D delete, Esc cancel)",
		items, defaultIdx, reader, cb,
	)
	if err != nil {
		return err
	}

	for _, msg := range actionMsgs {
		fmt.Println(msg)
	}

	if !confirmed || chosenItem.ID == "" {
		return nil
	}

	conv, err := sm.Resume(chosenItem.ID)
	if err != nil {
		return err
	}
	if err := sm.Save(); err != nil {
		return err
	}
	printResumedConversation(conv)
	return nil
}

// HandleJobs runs /jobs: it lists the background jobs and, when some are running, offers to
// stop one. A stopped job is told to the model with the user's next message.
func HandleJobs(reader *bufio.Reader) error {
	jobs := tools.Jobs.List()
	if len(jobs) == 0 {
		fmt.Println(theme.Muted("[no background jobs]"))
		return nil
	}
	var running []terminal.SelectItem
	for _, j := range jobs {
		fmt.Println(theme.Info(theme.Sanitize(j.Text)))
		if j.Running {
			running = append(running, terminal.SelectItem{ID: strconv.Itoa(j.ID), Text: theme.Sanitize(j.Text)})
		}
	}
	if len(running) == 0 {
		return nil
	}
	chosen, confirmed, err := terminal.InteractiveSelect(
		"stop a job (Enter stops it, Esc keeps them running)", running, 0, reader, nil,
	)
	if err != nil || !confirmed || chosen.ID == "" {
		return err
	}
	id, err := strconv.Atoi(chosen.ID)
	if err != nil {
		return fmt.Errorf("job %q: %w", chosen.ID, err)
	}
	if err := tools.Jobs.StopForUser(id); err != nil {
		return err
	}
	fmt.Println(theme.Muted(fmt.Sprintf("[job %d stopped; the model hears of it with your next message]", id)))
	return nil
}

// ShortcutsTable builds the terminal table for shortcuts and slash commands. In
// accessibility mode it leaves out the keys that do not work there.
func ShortcutsTable() *terminal.Table {
	rows := defaultShortcutRows()
	if theme.Accessibility() {
		rows = accessibleShortcutRows()
	}
	t := &terminal.Table{
		Headers: []string{"Shortcut", "Command", "Action"},
		Align:   []terminal.Alignment{terminal.AlignLeft, terminal.AlignLeft, terminal.AlignLeft},
		Rows:    rows,
	}
	for _, row := range t.Rows {
		for i := range row {
			row[i] = theme.ASCII(row[i])
		}
	}
	return t
}

// defaultShortcutRows is the shortcut table of the key-driven interactive mode.
func defaultShortcutRows() [][]string {
	return [][]string{
		{"`Ctrl+N`", "`/new`", "New conversation (clears the screen; earlier ones stay in `/resume`)"},
		{"`Ctrl+R`", "`/resume`", "Resume & manage conversations (`Ctrl+E` rename, `Ctrl+D` delete)"},
		{"`Ctrl+O`", "`/models`", "Switch model (interactive filter & select)"},
		{"`Ctrl+Y`", "`/history`", "Browse turns & copy prompt or AI output to clipboard"},
		{"`↑` / `↓`", "—", "Cycle prompt history (recalls previous prompts, restores draft)"},
		{"`Esc` / `Ctrl+C`", "—", "Halt running agent or tool execution"},
		{"`Ctrl+B`", "—", "Move a running command to the background, so you can keep chatting"},
		{"`Ctrl+D`", "—", "Exit HMJ Agent"},
		{"—", "`/jobs`", "List background jobs and stop one"},
		{"—", "`/health`", "Check health & latency of all configured providers and models"},
		{"—", "`/shortcuts`", "Display this shortcuts & commands table"},
	}
}

// accessibleShortcutRows is the shortcut table for accessibility mode: the Ctrl+ editing and
// history keys are omitted, the slash commands stay, and the multi-line rule is noted.
func accessibleShortcutRows() [][]string {
	return [][]string{
		{"—", "`/new`", "New conversation"},
		{"—", "`/resume`", "Resume & manage conversations (`r <n> <name>` renames, `d <n>` deletes)"},
		{"—", "`/models`", "Switch model (interactive filter & select)"},
		{"—", "`/history`", "Browse turns & copy prompt or AI output to clipboard"},
		{"`Esc` / `Ctrl+C`", "—", "Halt running agent or tool execution"},
		{"`Ctrl+D`", "—", "Exit HMJ Agent"},
		{"—", "—", "End a line with `\\` to continue the prompt on the next line"},
		{"—", "`/jobs`", "List background jobs and stop one"},
		{"—", "`/health`", "Check health & latency of all configured providers and models"},
		{"—", "`/shortcuts`", "Display this shortcuts & commands table"},
	}
}

// PrintBanner draws the startup banner (title + shortcuts table).
func PrintBanner() {
	fmt.Printf("%s\n", theme.Title("HMJ Agent "+config.VersionString()))
	fmt.Print(ShortcutsTable().Render())
	fmt.Println()
}

// PrintShortcuts prints the formatted shortcuts and commands table.
func PrintShortcuts() {
	fmt.Println()
	fmt.Printf("%s\n", theme.Title(theme.ASCII("HMJ Agent — Shortcuts & Commands")))
	fmt.Print(ShortcutsTable().Render())
	fmt.Println()
}

// Turn represents a user prompt paired with its assistant response.
type Turn struct {
	Index    int
	Prompt   string
	Response string
	// Compacted marks a turn that a compaction replaced: it is kept for /history, but the
	// model no longer sees it.
	Compacted bool
	// parts are the pieces Response joins, so the model's text can be shown rendered and the
	// tool calls and results as written.
	parts []turnPart
}

// turnPart is one piece of a turn's response: the model's text, or a tool call or result.
type turnPart struct {
	text  string
	model bool
}

// viewText returns the response as "View full turn" shows it: the model's Markdown rendered,
// tool calls and results as written.
func (t *Turn) viewText() string {
	if len(t.parts) == 0 {
		return theme.Sanitize(t.Response)
	}
	pieces := make([]string, len(t.parts))
	for i, p := range t.parts {
		pieces[i] = theme.Sanitize(p.text)
		if p.model {
			pieces[i] = terminal.RenderMarkdown(pieces[i])
		}
	}
	return strings.Join(pieces, "\n\n")
}

// HistoryTurns returns every turn of conv for /history: the turns compactions replaced, marked
// Compacted, then the turns in the conversation. When the replaced turns cannot be read in full,
// the turns read are returned with the error.
func HistoryTurns(conv *session.Conversation) ([]Turn, error) {
	earlier, err := conv.EarlierMessages()
	turns := turnsOf(earlier)
	for i := range turns {
		turns[i].Compacted = true
	}
	for _, t := range turnsOf(conv.Messages) {
		t.Index = len(turns) + 1
		turns = append(turns, t)
	}
	return turns, err
}

// turnsOf extracts the user-assistant turns from msgs.
func turnsOf(msgs []chat.Message) []Turn {
	var turns []Turn
	var curTurn *Turn
	toolNames := make(map[string]string)

	appendTurnResponse := func(text string, model bool) {
		if curTurn == nil || text == "" {
			return
		}
		curTurn.parts = append(curTurn.parts, turnPart{text, model})
		if curTurn.Response != "" {
			curTurn.Response += "\n\n" + text
		} else {
			curTurn.Response = text
		}
	}

	for _, m := range msgs {
		switch m.Role {
		case "user":
			if curTurn != nil {
				turns = append(turns, *curTurn)
			}
			curTurn = &Turn{
				Index:  len(turns) + 1,
				Prompt: m.Content,
			}
		case "assistant":
			if curTurn != nil {
				if m.Content != "" {
					appendTurnResponse(m.Content, true)
				}
				for _, tc := range m.ToolCalls {
					toolNames[tc.ID] = tc.Function.Name
					args := tc.Function.Arguments
					if args == "" {
						args = "{}"
					}
					glyph := "⚙ "
					if theme.Accessibility() {
						glyph = "- "
					}
					appendTurnResponse(fmt.Sprintf("%s%s: %s", glyph, tc.Function.Name, args), false)
				}
			}
		case "tool":
			if curTurn != nil {
				tName := toolNames[m.ToolCallID]
				if tName == "" {
					tName = "tool"
				}
				out := strings.TrimSpace(m.Content)
				if theme.Accessibility() {
					appendTurnResponse(resultBlock(tName, out), false)
				} else {
					appendTurnResponse(fmt.Sprintf("│ %s: %s", tName, out), false)
				}
			}
		}
	}
	if curTurn != nil {
		turns = append(turns, *curTurn)
	}
	return turns
}

// turnSnippet collapses a prompt onto one line and truncates it to limit runes,
// ending in "..." when it is cut.
func turnSnippet(prompt string, limit int) string {
	snippet := strings.TrimSpace(strings.ReplaceAll(prompt, "\n", " "))
	if len([]rune(snippet)) > limit {
		snippet = string([]rune(snippet)[:limit-3]) + "..."
	}
	return snippet
}

// buildTurnItems converts the turns into selector items labelled "[i/n] snippet", with
// "(compacted)" before the snippet of a turn a compaction replaced.
func buildTurnItems(turns []Turn) []terminal.SelectItem {
	var items []terminal.SelectItem
	for _, t := range turns {
		mark := ""
		if t.Compacted {
			mark = "(compacted) "
		}
		items = append(items, terminal.SelectItem{
			ID:   strconv.Itoa(t.Index),
			Text: fmt.Sprintf("[%d/%d] %s%s", t.Index, len(turns), mark, turnSnippet(t.Prompt, 60)),
		})
	}
	return items
}

// findTurn returns the turn whose index matches id, or nil when none does.
func findTurn(turns []Turn, id string) *Turn {
	for i := range turns {
		if strconv.Itoa(turns[i].Index) == id {
			return &turns[i]
		}
	}
	return nil
}

// runTurnAction carries out the chosen turn action: copy to the clipboard or view in
// the terminal. total is the number of turns in the conversation, used in messages.
func runTurnAction(actionID string, turn *Turn, total int) error {
	switch actionID {
	case "copy_output":
		if err := terminal.CopyToClipboard(turn.Response); err != nil {
			return err
		}
		fmt.Printf("%s %s\n",
			theme.OK(fmt.Sprintf("copied AI output of turn [%d/%d] to clipboard", turn.Index, total)),
			theme.Muted(fmt.Sprintf("(%d bytes)", len(turn.Response))))
	case "copy_prompt":
		if err := terminal.CopyToClipboard(turn.Prompt); err != nil {
			return err
		}
		fmt.Println(theme.OK(fmt.Sprintf("copied prompt of turn [%d/%d] to clipboard", turn.Index, total)))
	case "copy_both":
		md := fmt.Sprintf("## User\n\n%s\n\n## Assistant\n\n%s\n", turn.Prompt, turn.Response)
		if err := terminal.CopyToClipboard(md); err != nil {
			return err
		}
		fmt.Println(theme.OK(fmt.Sprintf("copied full turn [%d/%d] to clipboard (Markdown)", turn.Index, total)))
	case "view":
		fmt.Printf("\n%s %s\n", theme.Prompt("> you:"), theme.Text(turn.Prompt))
		if turn.Response != "" {
			fmt.Printf("\n%s\n\n", turn.viewText())
		}
	}
	return nil
}

// HandleHistory opens an interactive browser over conversation turns, including those
// compactions replaced, allowing the user to scroll through prompts and copy either prompt or AI
// output.
func HandleHistory(conv *session.Conversation, reader *bufio.Reader) error {
	turns, err := HistoryTurns(conv)
	if err != nil {
		logging.L().Warn("earlier turns not read", "error", err.Error())
		fmt.Println(theme.Caution(fmt.Sprintf("[could not read all the turns before the compaction: %s]", theme.Sanitize(err.Error()))))
	}
	if len(turns) == 0 {
		fmt.Println(theme.Muted("no prompt history in current conversation yet"))
		return nil
	}

	chosenItem, confirmed, err := terminal.InteractiveSelect(
		"browse turns (type to filter, ↑/↓ navigate, Enter select, Esc cancel)",
		buildTurnItems(turns), len(turns)-1, reader, nil,
	)
	if err != nil || !confirmed || chosenItem.ID == "" {
		return err
	}

	selectedTurn := findTurn(turns, chosenItem.ID)
	if selectedTurn == nil {
		return nil
	}

	actionItems := []terminal.SelectItem{
		{ID: "copy_output", Text: "Copy AI Output to clipboard"},
		{ID: "copy_prompt", Text: "Copy User Prompt to clipboard"},
		{ID: "copy_both", Text: "Copy Both (Turn Markdown) to clipboard"},
		{ID: "view", Text: "View full turn in terminal"},
	}

	actionChoice, confirmed, err := terminal.InteractiveSelect(
		fmt.Sprintf("turn [%d/%d] %q — choose action", selectedTurn.Index, len(turns), turnSnippet(selectedTurn.Prompt, 45)),
		actionItems, 0, reader, nil,
	)
	if err != nil || !confirmed || actionChoice.ID == "" {
		return err
	}

	return runTurnAction(actionChoice.ID, selectedTurn, len(turns))
}
