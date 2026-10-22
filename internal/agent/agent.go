package agent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"hmj-agent/internal/audit"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/diff"
	"hmj-agent/internal/logging"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
	"hmj-agent/internal/tools"
)

// maxLoggedArgBytes caps the tool arguments written to the diagnostic log.
const maxLoggedArgBytes = 1000

type Runner struct {
	Client   *chat.Client
	Tools    map[string]tools.Tool
	ToolDefs []chat.ToolDefinition
}

func NewRunner(client *chat.Client, cfg config.Config) *Runner {
	return NewRunnerWithTools(client, cfg, tools.DefaultTools())
}

// NewRunnerWithTools builds a runner with an explicit tool set, allowing tests
// and embeddings to restrict or extend the available tools.
func NewRunnerWithTools(client *chat.Client, cfg config.Config, toolList []tools.Tool) *Runner {
	if client != nil && client.OnRetry == nil {
		client.OnRetry = func(attempt, maxRetries int, delay time.Duration, err error) {
			// For long waits (>= 1 min), show the wall-clock time directly
			// (e.g. "retry 1/3 at 19:57:34") instead of raw seconds.
			var header string
			if delay >= time.Minute {
				header = fmt.Sprintf("[retry %d/%d at %s]:", attempt, maxRetries, time.Now().Add(delay).Format("15:04:05"))
			} else {
				header = fmt.Sprintf("[retry %d/%d in %s]:", attempt, maxRetries, FormatDuration(delay))
			}
			// The spinner may be drawing on the main stdout line from its
			// own goroutine; erase it while this notice prints, then let it
			// repaint on the fresh line so its animation continues cleanly.
			terminal.PauseActiveSpinner()
			fmt.Fprintf(os.Stderr, "\n%s %s\n",
				theme.Caution(header),
				theme.Muted(err.Error()),
			)
			terminal.ResumeActiveSpinner()
		}
	}

	if client != nil && client.OnProtocolDetected == nil {
		client.OnProtocolDetected = func(provider, api string, saved bool) {
			terminal.PauseActiveSpinner()
			fmt.Fprintln(os.Stderr, theme.Muted(ProtocolDetectedNotice(provider, api, saved)))
			terminal.ResumeActiveSpinner()
		}
	}

	toolsMap := make(map[string]tools.Tool, len(toolList))
	var toolDefs []chat.ToolDefinition
	for _, t := range toolList {
		toolsMap[t.Name()] = t
		toolDefs = append(toolDefs, t.Definition())
	}

	return &Runner{
		Client:   client,
		Tools:    toolsMap,
		ToolDefs: toolDefs,
	}
}

// Run executes one user turn: it records input, then streams model replies and runs any
// requested tools until the model answers without tool calls, the turn is cancelled or an
// error occurs.
func (r *Runner) Run(ctx context.Context, conv *session.Conversation, input string) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stopHaltListener := terminal.StartHaltListener(cancel)
	defer stopHaltListener()

	r.beginTurn(conv, input)

	fmt.Println()

	// Immediate feedback that the prompt was accepted: a quiet spinner keeps
	// the user informed while the request travels and until the first token
	// (or tool activity) arrives. Essential when network latency or retry
	// backoff delays time-to-first-token by seconds.
	spinner := terminal.NewSpinner("thinking…")
	spinner.Start()
	defer spinner.Stop()

	stats := &turnStats{turnStart: time.Now()}
	for step := 1; ; step++ {
		stats.totalSteps = step

		select {
		case <-runCtx.Done():
			stats.printFooter()
			return nil
		default:
		}

		stepStart := time.Now()
		assistantMsg, usage, streamedAny, err := r.streamStep(runCtx, conv, spinner)
		stats.add(usage)

		if err != nil {
			if errors.Is(runCtx.Err(), context.Canceled) {
				stats.printFooter()
				return nil
			}
			conv.Messages = append(conv.Messages, chat.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("[error: %v]", err),
			})
			return err
		}

		logStep(conv.Model, usage, time.Since(stepStart))
		recordPromptTokens(conv, usage)
		conv.Messages = append(conv.Messages, assistantMsg)

		if streamedAny {
			fmt.Println()
		}
		if usage != nil && usage.FinishReason == "refusal" {
			printRefusalNotice(usage.RefusalCategory)
		}

		if len(assistantMsg.ToolCalls) == 0 {
			stats.printFooter()
			r.maybeGenerateTitle(runCtx, conv, input, assistantMsg.Content)
			return nil
		}

		for i, tc := range assistantMsg.ToolCalls {
			select {
			case <-runCtx.Done():
				spinner.Stop()
				// Every tool call in the assistant message needs a matching tool result, or
				// the provider rejects the next request. Answer this call and the rest.
				appendCancelledToolResults(conv, assistantMsg.ToolCalls[i:])
				stats.printFooter()
				return nil
			default:
			}

			// A step can carry tool calls with no streamed text at all, so
			// the spinner may still be active when tool output begins.
			spinner.Stop()
			conv.Messages = append(conv.Messages, chat.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    r.runTool(runCtx, tc),
			})
		}
	}
}

// appendCancelledToolResults appends a placeholder tool result for each of calls, so the
// transcript stays valid for the provider after a halt.
func appendCancelledToolResults(conv *session.Conversation, calls []chat.ToolCall) {
	for _, tc := range calls {
		conv.Messages = append(conv.Messages, chat.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Content:    session.CancelledToolResult,
		})
	}
}

// recordPromptTokens stores the prompt size the provider reported for the latest request, which
// later estimates use as their baseline. A zero count is ignored.
func recordPromptTokens(conv *session.Conversation, usage *chat.TokenUsage) {
	if usage != nil && usage.PromptTokens > 0 {
		conv.SetPromptTokens(usage.PromptTokens)
	}
}

// printRefusalNotice tells the user the model declined the request, naming the safety category
// when the provider gave one.
func printRefusalNotice(category string) {
	notice := "[the model declined this request (safety classifier)]"
	if category != "" {
		notice = fmt.Sprintf("[the model declined this request (safety classifier: %s)]", theme.Sanitize(category))
	}
	fmt.Println(theme.Caution(notice))
}

// beginTurn prepares conv for a new prompt: it names an untitled conversation and appends the
// user's input. The system prompt is set only when the conversation has no user message yet, so a
// later change to it never invalidates the cached prefix or the reasoning blocks. The history is
// otherwise only appended to; compaction happens between turns (see StartCompaction).
func (r *Runner) beginTurn(conv *session.Conversation, input string) {
	if conv.UserMessageCount() == 0 {
		conv.SyncSystemPrompt(session.CurrentSystemPrompt())
	}
	conv.MaybeAutoTitle(input)
	conv.Messages = append(conv.Messages, chat.Message{
		Role:    "user",
		Content: input,
	})
}

// streamStep makes one model request, printing reasoning and answer text as it arrives.
// It reports whether any answer text was streamed.
func (r *Runner) streamStep(ctx context.Context, conv *session.Conversation, spinner *terminal.Spinner) (chat.Message, *chat.TokenUsage, bool, error) {
	var streamedAny, streamedThnk bool

	// The spinner stays visible across the whole blocking StreamChat
	// call (connect, retries, TTFT); the first streamed byte stops it.
	spinner.Start()

	tableStreamer := terminal.NewTableStreamer(func(s string) {
		spinner.Stop()
		fmt.Print(s)
	})

	assistantMsg, usage, err := r.Client.StreamChat(
		ctx,
		conv.Model,
		conv.Messages,
		r.ToolDefs,
		func(thought string) {
			spinner.Stop()
			if !streamedThnk {
				thoughtLabel := "▾ Thought Process"
				if theme.Accessibility() {
					thoughtLabel = "Thought Process"
				}
				fmt.Print(theme.Thinking() + thoughtLabel + "\n  ")
				streamedThnk = true
			}
			fmt.Print(theme.Sanitize(thought))
		},
		func(delta string) {
			// Stop on the first delta, not on the first flushed line:
			// the table streamer buffers until a newline, which could
			// keep the spinner running while tokens are already flowing.
			spinner.Stop()
			if streamedThnk {
				endThoughts()
				streamedThnk = false
			}
			tableStreamer.Write(theme.Sanitize(delta))
			streamedAny = true
		},
	)

	tableStreamer.Flush()
	spinner.Stop()

	if streamedThnk {
		endThoughts()
	}

	return assistantMsg, usage, streamedAny, err
}

// endThoughtsLabel is the line that closes the reasoning block in accessibility mode, so a screen
// reader announces where the thoughts end and the answer begins.
const endThoughtsLabel = "[END THOUGHTS]"

// endThoughts closes the reasoning block. Accessibility mode prints the end label on its own line;
// the normal view keeps a blank line.
func endThoughts() {
	if theme.Accessibility() {
		fmt.Print(theme.Reset() + "\n" + endThoughtsLabel + "\n")
		return
	}
	fmt.Print(theme.Reset() + "\n\n")
}

// runTool prints a tool call, runs it and prints the result. It returns the output
// that is sent back to the model.
func (r *Runner) runTool(ctx context.Context, tc chat.ToolCall) string {
	toolName := tc.Function.Name
	toolArgs := tc.Function.Arguments
	printExpandedToolCall(toolName, toolArgs)

	tool, exists := r.Tools[toolName]
	if !exists {
		output := fmt.Sprintf("error: unknown tool %q", toolName)
		printExpandedToolResult(theme.Sanitize(toolName), output)
		return output
	}

	// Accessibility mode reports long-running tools on their own lines; the ticker
	// is stopped before the result is printed, and the defer covers any other exit.
	stopProgress := terminal.StartAccessibleProgress(toolName)
	defer stopProgress()
	toolStart := time.Now()
	res, execErr := execTool(ctx, tool, toolArgs)
	toolDuration := time.Since(toolStart)
	stopProgress()

	output := res
	if execErr != nil {
		output = fmt.Sprintf("error: %v", execErr)
	}
	logTool(toolName, toolArgs, toolDuration, output, execErr)
	audit.Record(toolName, toolArgs, toolDuration, len(output), execErr)
	printExpandedToolResult(toolName, output, toolDuration)
	return output
}

// logTool records one tool execution in the diagnostic log. Arguments are truncated
// and the error attribute is present only when the tool failed.
func logTool(name, args string, d time.Duration, output string, execErr error) {
	attrs := []any{
		"name", name,
		"args", logging.Truncate(args, maxLoggedArgBytes),
		"duration", d,
		"output_bytes", len(output),
	}
	if execErr != nil {
		attrs = append(attrs, "error", execErr.Error())
	}
	logging.L().Info("tool", attrs...)
}

// logStep records a completed model step in the diagnostic log. usage may be nil
// when the provider reported no token counts.
func logStep(ref config.ModelRef, usage *chat.TokenUsage, d time.Duration) {
	attrs := []any{"provider", ref.Provider, "model", ref.Model, "duration", d}
	if usage != nil {
		attrs = append(attrs,
			"prompt_tokens", usage.PromptTokens,
			"completion_tokens", usage.CompletionTokens,
			"cached_tokens", usage.EffectiveCachedTokens(),
			"ttft", usage.TTFT,
			"finish_reason", usage.FinishReason,
		)
		if usage.Speed != "" {
			attrs = append(attrs, "speed", usage.Speed)
		}
	}
	logging.L().Debug("llm step", attrs...)
}

// execTool runs tool and turns a panic into an error, so a faulty tool cannot end the turn.
// The full stack goes to stderr for debugging; the model only receives the concise message.
func execTool(ctx context.Context, tool tools.Tool, args string) (res string, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			fmt.Fprintf(os.Stderr, "[tool panic stack]\n%s\n", stack)
			logging.L().Error("tool panic", "name", tool.Name(), "panic", fmt.Sprint(r), "stack", string(stack))
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	return tool.Execute(ctx, args)
}

// turnStats accumulates token counts and timing across the model steps of one user turn.
type turnStats struct {
	hasUsage         bool
	lastPromptTokens int
	lastCachedTokens int
	sumCompletion    int
	sumTotalTokens   int
	totalSteps       int
	lastTTFT         time.Duration
	sumGenDuration   time.Duration
	turnStart        time.Time
	wasTruncated     bool
	// speedSteps counts the steps whose provider reported a speed, and fastSteps those that ran
	// in fast mode.
	speedSteps int
	fastSteps  int
}

// add folds one step's usage into the turn totals. A nil usage is ignored.
func (s *turnStats) add(usage *chat.TokenUsage) {
	if usage == nil {
		return
	}
	s.hasUsage = true
	s.lastPromptTokens = usage.PromptTokens
	// Input is the latest step's prompt, so the cached count is that step's too.
	s.lastCachedTokens = usage.EffectiveCachedTokens()
	s.sumCompletion += usage.CompletionTokens
	s.sumTotalTokens += usage.TotalTokens
	if usage.TTFT > 0 {
		s.lastTTFT = usage.TTFT
	}
	if usage.Duration > usage.TTFT {
		s.sumGenDuration += (usage.Duration - usage.TTFT)
	}
	if usage.FinishReason == "length" {
		s.wasTruncated = true
	}
	if usage.Speed != "" {
		s.speedSteps++
		if usage.Speed == "fast" {
			s.fastSteps++
		}
	}
}

// printFooter prints the token and timing summary for the turn. It prints nothing when no
// step reported usage.
func (s *turnStats) printFooter() {
	if !s.hasUsage {
		return
	}

	// Input tokens and cached
	inputStr := fmt.Sprintf("%s input", FormatNumber(s.lastPromptTokens))
	if s.lastCachedTokens > 0 {
		inputStr += fmt.Sprintf(" (%s cached)", theme.Cached(FormatNumber(s.lastCachedTokens)))
	}

	// Output tokens
	outputStr := fmt.Sprintf("%s output", FormatNumber(s.sumCompletion))

	// Total tokens
	var totalStr string
	if s.totalSteps <= 1 {
		totalStr = theme.Bold(theme.Muted(fmt.Sprintf("%s total", FormatNumber(s.sumTotalTokens))))
	} else {
		totalStr = theme.Bold(theme.Muted(fmt.Sprintf("%s total", FormatNumber(s.sumTotalTokens)))) +
			" " + theme.Steps(s.totalSteps)
	}

	// Generation speed (tokens per second)
	var speedStr string
	if s.sumGenDuration > 0 && s.sumCompletion > 0 {
		speed := float64(s.sumCompletion) / s.sumGenDuration.Seconds()
		speedStr = fmt.Sprintf("%.1f tokens/s", speed)
	}

	// Total time and time to first token
	totalDuration := time.Since(s.turnStart)
	timeStr := fmt.Sprintf("%s total", FormatDuration(totalDuration))
	if s.lastTTFT > 0 && s.totalSteps <= 1 {
		timeStr += fmt.Sprintf(" (%s to first token)", FormatDuration(s.lastTTFT))
	}

	footerParts := []string{inputStr, outputStr, totalStr}
	if speedStr != "" {
		footerParts = append(footerParts, speedStr)
	}
	// Fast mode is named only when it was used, so standard-speed turns keep their footer.
	if s.fastSteps > 0 {
		fastStr := "fast mode"
		if s.fastSteps < s.speedSteps {
			fastStr += fmt.Sprintf(" in %d of %d steps", s.fastSteps, s.speedSteps)
		}
		footerParts = append(footerParts, fastStr)
	}
	footerParts = append(footerParts, timeStr)

	fmt.Printf("%s\n", theme.Muted("[")+theme.Accent("tokens:")+" "+strings.Join(footerParts, theme.FooterJoin())+theme.Muted("]"))

	if s.wasTruncated {
		fmt.Println(theme.TruncationNotice("[warning: output cut off by model maximum token limit]"))
	}
}

// PrintConversationHistory prints the transcript of a resumed conversation. A compaction is shown
// as a notice, followed by its summary when the provider's summary can be read.
func PrintConversationHistory(conv *session.Conversation) {
	toolNames := make(map[string]string)
	for _, m := range conv.Messages {
		for _, tc := range m.ToolCalls {
			toolNames[tc.ID] = tc.Function.Name
		}
	}

	var printedAny bool
	var lastRole string
	for _, m := range conv.Messages {
		switch m.Role {
		case "system":
			continue
		case "user":
			if lastRole != "" && lastRole != "user" {
				fmt.Println()
			}
			fmt.Printf("%s %s %s\n", theme.Prompt(">"), theme.User("you:"), theme.Text(theme.Sanitize(m.Content)))
			lastRole = "user"
			printedAny = true
		case "assistant":
			if b := m.CompactionBlock(); b != nil {
				fmt.Println(theme.Muted("[the earlier conversation was compacted]"))
				if summary := b.ReadableSummary(); summary != "" {
					fmt.Printf("\n%s\n", theme.Muted(theme.Sanitize(summary)))
				}
				lastRole = "assistant"
				printedAny = true
				continue
			}
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					printExpandedToolCall(tc.Function.Name, tc.Function.Arguments)
				}
			}
			if m.Content != "" {
				fmt.Printf("\n%s\n", theme.Sanitize(m.Content))
			}
			lastRole = "assistant"
			printedAny = true
		case "tool":
			tName := toolNames[m.ToolCallID]
			printExpandedToolResult(tName, m.Content)
			lastRole = "tool"
			printedAny = true
		}
	}
	if printedAny {
		fmt.Println()
	}
}

func printExpandedToolCall(name, argsJSON string) {
	// Every displayed field comes from the model, so escape sequences are stripped
	// before printing: a command must not be able to redraw itself as something else.
	badge := theme.ToolBadge(theme.Sanitize(name))
	var raw map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		fmt.Printf("\n%s %s %s\n", badge, theme.Muted("(unparseable arguments)"), theme.Muted(theme.Sanitize(argsJSON)))
		return
	}

	// Display-only extraction: missing or non-string argument fields render as
	// empty strings in the transcript; the actual tool execution below
	// re-parses args strictly and reports real errors.
	get := func(key string) string {
		s, ok := raw[key].(string)
		if !ok {
			return ""
		}
		return s
	}
	show := func(key string) string { return theme.Sanitize(get(key)) }

	switch name {
	case "read_file":
		path := show("path")
		if off, hasOff := raw["offset"]; hasOff {
			fmt.Printf("\n%s %s %s\n", badge, theme.FilePath(path), theme.Muted(fmt.Sprintf("(offset: %s)", theme.Sanitize(fmt.Sprint(off)))))
		} else {
			fmt.Printf("\n%s %s\n", badge, theme.FilePath(path))
		}

	case "write_file":
		path, content := show("path"), get("content")
		fmt.Printf("\n%s %s %s\n", badge, theme.FilePath(path), theme.Muted(fmt.Sprintf("(%d bytes)", len(content))))

	case "edit_file":
		printEditFileCall(get("path"), get("old_string"), get("new_string"))

	case "list_files":
		path := show("path")
		if path == "" {
			path = "."
		}
		fmt.Printf("\n%s %s\n", badge, theme.FilePath(path))

	case "run_command":
		fmt.Printf("\n%s %s\n", badge, theme.Info(show("command")))

	case "web_search":
		fmt.Printf("\n%s %s\n", badge, theme.Info(show("query")))

	case "fetch_url":
		fmt.Printf("\n%s %s\n", badge, theme.Info(show("url")))

	default:
		fmt.Printf("\n%s %s\n", badge, theme.Muted(theme.Sanitize(argsJSON)))
	}
}

// printEditFileCall renders an edit_file tool call as a real line-numbered diff
// with word-level highlighting. When the old text cannot be located in the
// file (or the call is a creation), it falls back to simple minus/plus blocks
// so the user always sees what will change.
func printEditFileCall(path, oldStr, newStr string) {
	fmt.Printf("\n%s %s\n", theme.ToolBadge("edit_file"), theme.FilePath(theme.Sanitize(path)))
	if oldStr == "" && newStr == "" {
		return
	}

	oldText, haveOld := readFileForDiff(path)
	var d *diff.File
	if haveOld && oldStr != "" && strings.Contains(oldText, oldStr) {
		newText := strings.Replace(oldText, oldStr, newStr, 1)
		d = diff.Text(path, oldText, newText, diff.Options{Inline: true, MaxLines: maxDiffRows})
	} else if !haveOld && oldStr == "" {
		// New file creation: show every line as an addition.
		d = diff.Text(path, "", EnsureTrailingNewline(newStr), diff.Options{Inline: false, MaxLines: maxDiffRows})
	}

	if d != nil && !d.Empty() {
		printDiff(d)
		return
	}

	// Fallback: naive minus/plus block (unreadable path, or not-found old_string).
	if oldStr != "" {
		for l := range strings.SplitSeq(strings.TrimRight(oldStr, "\n"), "\n") {
			fmt.Printf("  %s %s\n", theme.DelGlyph(), theme.RemovedBlock(" "+theme.Sanitize(l)+" "))
		}
	}
	if newStr != "" {
		for l := range strings.SplitSeq(strings.TrimRight(newStr, "\n"), "\n") {
			fmt.Printf("  %s %s\n", theme.AddGlyph(), theme.Added(theme.Sanitize(l)))
		}
	}
}

// maxDiffRows caps how many diff rows one tool call prints inline.
const maxDiffRows = 120

// readFileForDiff reads a file for diffing, returning ok=false when it does not
// exist or cannot be read (never an error: display must stay best-effort).
func readFileForDiff(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	absPath, err := tools.ResolvePath(path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// EnsureTrailingNewline appends a newline to s unless s is empty or already
// ends with one. Exported for testing.
func EnsureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// printDiff renders a computed diff with the theme palette.
func printDiff(d *diff.File) {
	paint := func(s string, kind diff.SegmentStyle) string {
		// Diff text is file content or model output: strip escape sequences so a
		// line cannot drive the terminal (cursor moves, OSC 52 clipboard writes).
		s = theme.Sanitize(s)
		switch kind {
		case diff.StyleLineNum:
			return theme.Faint(s)
		case diff.StyleGlyphAdd:
			return theme.AddGlyph()
		case diff.StyleGlyphDel:
			return theme.DelGlyph()
		case diff.StyleTextAdd:
			return s
		case diff.StyleMarkAdd:
			return theme.Mark(s)
		case diff.StyleTextDel:
			return s
		case diff.StyleMarkDel:
			return theme.Mark(s)
		case diff.StyleRule, diff.StyleFaint:
			return theme.Faint(s)
		case diff.StyleAccent:
			return theme.Accent(s)
		case diff.StylePath:
			return theme.FilePath(s)
		default:
			// Context lines: pure white text
			return theme.BrightWhite(s)
		}
	}

	// Single clean diff header with 2-space indent (DiffHeader gates the
	// separator glyph for accessibility mode).
	fmt.Printf("  %s\n", theme.DiffHeader(theme.Sanitize(d.Path), theme.FormatDiffStats(d.Added, d.Removed)))

	for i, row := range d.Render(paint) {
		if i == 0 {
			// Skip the redundant header row emitted by d.Render
			continue
		}
		if strings.Contains(row.Raw, "@@") {
			continue
		}
		if row.Kind == diff.Del {
			fmt.Printf("  %s\n", theme.RemovedLine(row.Number, row.Glyph, row.Body))
		} else if row.Kind == diff.Add {
			fmt.Printf("  %s\n", theme.AddedLine(row.Number, row.Glyph, row.Body))
		} else {
			if row.Notice && row.Body == "" {
				// Hunk separator: its "···" lives in Raw, not Body.
				fmt.Printf("  %s\n", theme.Faint(theme.ASCII(row.Raw)))
				continue
			}
			cleanNum := strings.TrimRight(theme.StripANSI(row.Number), " ")
			cleanBody := strings.TrimRight(theme.StripANSI(row.Body), " \t\r\n")
			if row.Notice {
				cleanBody = theme.ASCII(cleanBody)
			}
			fmt.Printf("  %s   %s\n", theme.Faint(cleanNum), theme.BrightWhite(cleanBody))
		}
	}
}

// PrintDiff renders a computed diff with the theme palette. Exported for testing.
func PrintDiff(d *diff.File) {
	printDiff(d)
}

// PrintEditFileCall renders an edit_file tool call as a real line-numbered diff. Exported for testing.
func PrintEditFileCall(path, oldStr, newStr string) {
	printEditFileCall(path, oldStr, newStr)
}

func printExpandedToolResult(toolName string, output string, duration ...time.Duration) {
	output = strings.TrimRight(output, "\r\n")
	var durStr string
	if len(duration) > 0 && duration[0] > 0 {
		durStr = theme.Muted(fmt.Sprintf("(%s)", FormatDuration(duration[0])))
	}

	if output == "" {
		if durStr != "" {
			fmt.Printf("  %s\n", durStr)
		}
		return
	}

	if strings.HasPrefix(output, "error:") {
		fmt.Fprintf(os.Stderr, "  %s\n", theme.Danger(theme.Sanitize(output)))
		if durStr != "" {
			fmt.Printf("  %s\n", durStr)
		}
		return
	}

	if toolName == "read_file" {
		lineCount := strings.Count(output, "\n") + 1
		lineStr := fmt.Sprintf("(%d lines)", lineCount)
		if output == "[empty file]" {
			lineStr = "(empty file)"
		}
		if durStr != "" {
			fmt.Printf("  %s %s %s\n", theme.OK("read"), theme.Muted(lineStr), durStr)
		} else {
			fmt.Printf("  %s %s\n", theme.OK("read"), theme.Muted(lineStr))
		}
		return
	}

	if theme.Accessibility() {
		fmt.Println(resultBlock(toolName, output))
	} else {
		for l := range strings.SplitSeq(output, "\n") {
			fmt.Printf("  %s %s\n", theme.Faint("│"), theme.Sanitize(l))
		}
	}
	if durStr != "" {
		fmt.Printf("  %s\n", durStr)
	}
}

// resultBlock frames a tool result for accessibility mode: a [RESULT name] header, the
// output indented by two spaces with no gutter, and an [END RESULT] footer. Without a name
// the header is [OUTPUT].
func resultBlock(name, output string) string {
	header := "[OUTPUT]"
	if name != "" {
		header = "[RESULT " + name + "]"
	}
	var sb strings.Builder
	sb.WriteString(header + "\n")
	for l := range strings.SplitSeq(output, "\n") {
		sb.WriteString("  " + theme.Sanitize(l) + "\n")
	}
	sb.WriteString("[END RESULT]")
	return sb.String()
}

func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func FormatNumber(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	s := strconv.Itoa(n)
	var result strings.Builder
	remainder := len(s) % 3
	if remainder > 0 {
		result.WriteString(s[:remainder])
		if len(s) > remainder {
			result.WriteString(",")
		}
	}
	for i := remainder; i < len(s); i += 3 {
		result.WriteString(s[i : i+3])
		if i+3 < len(s) {
			result.WriteString(",")
		}
	}
	return result.String()
}

// ProtocolDetectedNotice says which protocol a provider was found to speak and whether the
// config file was updated so it is not probed again.
func ProtocolDetectedNotice(provider, api string, saved bool) string {
	if saved {
		return fmt.Sprintf("[provider %s speaks the %s API; saved \"api\": %q to the config file]", provider, api, api)
	}
	return fmt.Sprintf("[provider %s speaks the %s API; the config file could not be updated, so it will be probed again next run]", provider, api)
}
