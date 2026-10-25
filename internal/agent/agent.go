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
	"sync/atomic"
	"time"

	"hmj-agent/internal/audit"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
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
	// progressEvery is how often accessibility mode reports a running tool; zero is the
	// terminal's default.
	progressEvery time.Duration
	// Checkpoint, when set, saves the conversation during a turn, where it can be sent again
	// as it stands: once the user's message is added and after each step's tool results. A
	// process killed mid-turn (out of memory, SIGKILL) then loses at most the step under way.
	Checkpoint func()
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
				theme.Muted(theme.Sanitize(err.Error())),
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
//
// Background jobs of conv that ended since the model last heard go first in the user's message:
// with input, or alone when input is empty, as when one woke the idle prompt. With neither
// there is no turn. So do jobs whose end the model never heard of because hmj stopped first.
func (r *Runner) Run(ctx context.Context, conv *session.Conversation, input string) error {
	notices := append(tools.Jobs.LostNotices(conv.ScratchDir, conv.Messages), tools.Jobs.TakeFinished(conv.ScratchDir)...)
	if input == "" && len(notices) == 0 {
		return nil
	}
	// Ctrl+B moves a running command to the background; a press while none runs is dropped
	// before the next tool starts.
	backgroundKeys := make(chan struct{}, 1)
	var movedJob atomic.Int64 // the job a Ctrl+B made of the running command, which ends the turn
	runCtx, cancel := context.WithCancel(tools.WithBackgroundKey(tools.WithScratchDir(ctx, conv.ScratchDir), (<-chan struct{})(backgroundKeys),
		func(job int) { movedJob.Store(int64(job)) }))
	defer cancel()

	// Immediate feedback that the prompt was accepted: a quiet spinner keeps
	// the user informed while the request travels and until the first token
	// (or tool activity) arrives. Essential when network latency or retry
	// backoff delays time-to-first-token by seconds.
	spinner := terminal.NewSpinner("thinking…")

	// The key that halted the turn is reported with its footer, after what the step or tool
	// it stopped printed on its way out; meanwhile the status line says it is stopping.
	var haltedBy atomic.Pointer[string]
	stopHaltListener := terminal.ListenForKeys(func(key string) {
		haltedBy.Store(&key)
		spinner.SetLabel("halting…")
		spinner.SetDetail("")
		cancel()
	}, func() {
		select {
		case backgroundKeys <- struct{}{}:
		default:
		}
	})
	defer stopHaltListener()

	r.beginTurn(conv, withJobNotices(notices, input))
	r.checkpoint(conv)

	// Every block the turn prints brings the blank line before it, so nothing is printed here
	// that a tool call's own blank line would double.
	if len(notices) > 0 {
		fmt.Println()
		printJobNotices(notices)
	}

	spinner.Start()
	defer spinner.Stop()

	stats := &turnStats{turnStart: time.Now()}
	var prevRun toolRun // the turn's latest tool call and its result
	// Every way out of the turn passes here, so it also marks when the conversation last changed.
	endTurn := func() {
		conv.UpdatedAt = time.Now()
		spinner.Stop()
		if key := haltedBy.Load(); key != nil {
			fmt.Printf("\n%s\n", terminal.HaltNotice(*key))
		}
		stats.printFooter()
	}
	for step := 1; ; step++ {
		stats.totalSteps = step
		spinner.SetLabel("thinking…")
		spinner.SetStatus(stats.liveStatus())
		spinner.SetDetail("")

		select {
		case <-runCtx.Done():
			endTurn()
			return nil
		default:
		}

		stepStart := time.Now()
		assistantMsg, usage, err := r.streamStep(runCtx, conv, spinner)
		// A request over the model's context limit is sent again without the largest result
		// of this step's tool calls, until it fits or no large result is left. A result that
		// is removed stays removed even if the request still fails, so the saved conversation
		// can be sent again.
		for chat.IsContextOverflow(err) && runCtx.Err() == nil {
			name, size, ok := removeLargestStepResult(conv)
			if !ok {
				break
			}
			fmt.Println(theme.Caution(removedResultNotice(name, size)))
			assistantMsg, usage, err = r.streamStep(runCtx, conv, spinner)
			logging.L().Warn("tool result removed", "name", name, "bytes", size, "retry_ok", err == nil)
		}
		stats.add(usage)
		spinner.SetStatus(stats.liveStatus()) // this step's tools show its usage too

		if err != nil {
			// The steps before this one were paid for, so the footer shows them either way.
			endTurn()
			if errors.Is(runCtx.Err(), context.Canceled) {
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

		if usage != nil && usage.FinishReason == "refusal" {
			printRefusalNotice(usage.RefusalCategory)
		}

		if len(assistantMsg.ToolCalls) == 0 {
			endTurn()
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
				endTurn()
				return nil
			default:
			}

			// A step can carry tool calls with no streamed text at all, so
			// the spinner may still be active when tool output begins.
			spinner.Stop()
			select {
			case <-backgroundKeys:
			default:
			}
			spinner.SetStatus(stats.liveStatus())
			if movableToBackground(tc) {
				spinner.SetStatus(strings.TrimPrefix(stats.liveStatus()+" · Ctrl+B: background", " · "))
			}
			output, images := r.runTool(runCtx, tc, spinner)
			run := toolRun{tc.Function.Name, tc.Function.Arguments, output}
			if len(images) == 0 && run.repeats(prevRun) {
				output += repeatedCallNote
			}
			prevRun = run
			// A background job that ended meanwhile is told with this result, so the model
			// hears of it at its next step.
			if notices := tools.Jobs.TakeFinished(conv.ScratchDir); len(notices) > 0 {
				printJobNotices(notices)
				output += "\n\n" + strings.Join(notices, "\n\n")
			}
			conv.Messages = append(conv.Messages, chat.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
				Images:     images,
			})
			// The user pressed Ctrl+B to have the prompt back, so the turn ends here, as a halt
			// does; the model hears of the job with the next message or when it ends.
			if job := movedJob.Swap(0); job != 0 {
				appendCancelledToolResults(conv, assistantMsg.ToolCalls[i+1:])
				spinner.Stop()
				fmt.Printf("\n%s\n", theme.Info(fmt.Sprintf("[job %d runs in the background; the model hears of it with your next message, or when it ends]", job)))
				endTurn()
				return nil
			}
		}
		r.checkpoint(conv)
	}
}

// checkpoint marks conv as changed and calls r.Checkpoint, if set.
func (r *Runner) checkpoint(conv *session.Conversation) {
	if r.Checkpoint != nil {
		conv.UpdatedAt = time.Now()
		r.Checkpoint()
	}
}

// withJobNotices puts the finish notices of background jobs before the user's input.
func withJobNotices(notices []string, input string) string {
	if len(notices) == 0 {
		return input
	}
	text := strings.Join(notices, "\n\n")
	if input != "" {
		text += "\n\n" + input
	}
	return text
}

// printJobNotices shows the user the first line of each finish notice the model is given.
func printJobNotices(notices []string) {
	for _, n := range notices {
		line, _, _ := strings.Cut(n, "\n")
		fmt.Println(theme.Info(theme.Sanitize(line)))
	}
}

// movableToBackground reports whether tc is a foreground run_command, which Ctrl+B can move
// to the background.
func movableToBackground(tc chat.ToolCall) bool {
	if tc.Function.Name != "run_command" {
		return false
	}
	var args struct {
		Background bool `json:"background"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return true // the tool refuses arguments it cannot read, in the foreground
	}
	return !args.Background
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

// beginTurn prepares conv for a new prompt: it creates its scratch directory, names an untitled
// conversation and appends the user's input. The system prompt, which names the scratch
// directory, is set only when the conversation has no user message yet, so a later change to it
// never invalidates the cached prefix or the reasoning blocks. The history is otherwise only
// appended to; compaction happens between turns (see StartCompaction).
func (r *Runner) beginTurn(conv *session.Conversation, input string) {
	scratch := conv.ScratchDir
	if scratch != "" && os.MkdirAll(scratch, 0o700) != nil {
		scratch = "" // not offered if it cannot be made
	}
	if conv.UserMessageCount() == 0 {
		prompt := session.CurrentSystemPrompt()
		if scratch != "" {
			prompt += session.ScratchPrompt(scratch)
		}
		conv.SyncSystemPrompt(prompt)
	}
	conv.MaybeAutoTitle(input)
	conv.Messages = append(conv.Messages, chat.Message{
		Role:    "user",
		Content: input,
	})
}

// streamStep makes one model request, printing answer text as it arrives. While the model
// reasons, the status line shows the sentence it is writing; once it is done, the transcript
// keeps one line saying how long it thought. While it writes a tool call, the status line names
// the tool and the size of its arguments so far.
//
// Each block the step prints, the line saying how long the model thought or a run of answer
// text, has one blank line before it, as tool calls do. The text's own trailing line breaks are
// dropped, and the step ends its last line.
func (r *Runner) streamStep(ctx context.Context, conv *session.Conversation, spinner *terminal.Spinner) (chat.Message, *chat.TokenUsage, error) {
	blockStart := func() { fmt.Println() }
	var thinking *thoughtLine // set while the model reasons
	// Reasoning is timed from the request, or from the answer text before it: a provider may
	// send its summary of the reasoning only once the reasoning is done.
	quietSince := time.Now()
	endThoughts := func() {
		if thinking != nil {
			spinner.Stop()
			spinner.SetDetail("")
			blockStart()
			printThoughtDuration(time.Since(thinking.start))
			thinking = nil
		}
	}

	// The spinner stays visible across the whole blocking StreamChat
	// call (connect, retries, TTFT); the first streamed byte stops it.
	spinner.Start()

	inText := false     // whether a run of answer text is being printed
	atLineStart := true // whether the answer text printed so far ends a line
	heldBreaks := ""    // the text's trailing line breaks, printed only if more text follows
	mdStreamer := terminal.NewMarkdownStreamer(func(s string) {
		spinner.Stop()
		s = heldBreaks + s
		body := strings.TrimRight(s, "\n")
		heldBreaks = s[len(body):]
		if body != "" {
			fmt.Print(body)
			atLineStart = false
		}
	})
	// endText prints the answer text held back for its line break and ends its line, so that
	// a status line or another block can follow it.
	endText := func() {
		mdStreamer.Flush()
		if !atLineStart {
			fmt.Println()
			atLineStart = true
		}
		heldBreaks = ""
		inText = false
	}

	// A tool call's arguments can take tens of seconds to write, a large write_file's above
	// all, and nothing else would show meanwhile.
	writing, written := "", 0 // the tool call being written and its arguments' size so far
	ctx = chat.WithToolCallProgress(ctx, func(name string, argBytes int) {
		if name != writing || argBytes < written { // a new call
			endThoughts()
			endText()
			spinner.Stop() // restarts the elapsed time
			writing = name
			spinner.SetLabel("writing " + progressLine(name))
			spinner.SetDetail("")
			spinner.Start()
		}
		written = argBytes
		if argBytes > 0 {
			spinner.SetDetail(formatBytes(argBytes))
		}
	})

	assistantMsg, usage, err := r.Client.StreamChat(
		ctx,
		conv.Model,
		conv.Messages,
		r.ToolDefs,
		func(thought string) {
			if thinking == nil {
				// Answer text before the reasoning is printed first, so the status line and
				// then the reasoning's line come after it.
				endText()
				thinking = &thoughtLine{start: quietSince}
				spinner.SetLabel("thinking…")
				spinner.Start()
			}
			spinner.SetDetail(thinking.add(thought))
		},
		func(delta string) {
			// Stop on the first delta, not on the first flushed line:
			// the Markdown streamer buffers until a newline, which could
			// keep the spinner running while tokens are already flowing.
			endThoughts()
			spinner.Stop()
			writing = ""
			if !inText {
				blockStart()
				inText = true
			}
			mdStreamer.Write(theme.Sanitize(delta))
			quietSince = time.Now()
		},
	)

	endText()
	endThoughts()
	spinner.Stop()

	return assistantMsg, usage, err
}

// runTool prints a tool call, runs it and prints the result. It returns the output
// that is sent back to the model. While the tool runs, spinner shows its name, the time
// it has taken and its latest output line.
func (r *Runner) runTool(ctx context.Context, tc chat.ToolCall, spinner *terminal.Spinner) (string, []chat.Image) {
	toolName := tc.Function.Name
	toolArgs := tc.Function.Arguments
	quiet := quietTools[toolName]
	if !quiet {
		printExpandedToolCall(toolName, toolArgs)
	}

	tool, exists := r.Tools[toolName]
	if !exists {
		output := fmt.Sprintf("error: unknown tool %q", toolName)
		printExpandedToolResult(theme.Sanitize(toolName), output)
		return output, nil
	}

	// Accessibility mode reports long-running tools on their own lines; the ticker
	// is stopped before the result is printed, and the defer covers any other exit.
	stopProgress := terminal.StartAccessibleProgress(toolName, r.progressEvery)
	defer stopProgress()
	if !theme.Accessibility() {
		label := toolName
		if quiet { // its call line is printed once it has finished
			label = progressLine(theme.StripANSI(toolCallLine(toolName, toolArgs)))
		}
		spinner.SetLabel(label)
		spinner.SetDetail("")
		ctx = tools.WithProgress(ctx, func(line string) { spinner.SetDetail(progressLine(line)) })
		spinner.Start()
		defer spinner.Stop()
	}
	toolStart := time.Now()
	res, execErr := execTool(ctx, tool, toolArgs)
	toolDuration := time.Since(toolStart)
	stopProgress()
	spinner.Stop()

	output := res.Text
	if execErr != nil {
		output = fmt.Sprintf("error: %v", execErr)
		res.Images = nil
	}
	logTool(toolName, toolArgs, toolDuration, output, execErr)
	audit.Record(toolName, toolArgs, toolDuration, len(output), execErr)
	// The terminal shows what the model gets; the logs above keep the full size.
	output = capToolResult(toolName, output)
	if quiet {
		printQuietToolResult(toolName, toolArgs, output, len(res.Images) > 0, toolDuration)
	} else {
		printExpandedToolResult(toolName, output, toolDuration)
	}
	return output, res.Images
}

// progressLine makes a tool's output line fit the status line: escape sequences and control
// characters are removed, and runs of spaces and tabs become one space.
func progressLine(line string) string {
	return strings.Join(strings.Fields(theme.Sanitize(line)), " ")
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
func execTool(ctx context.Context, tool tools.Tool, args string) (res tools.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			fmt.Fprintf(os.Stderr, "[tool panic stack]\n%s\n", stack)
			logging.L().Error("tool panic", "name", tool.Name(), "panic", fmt.Sprint(r), "stack", string(stack))
			err = fmt.Errorf("tool panicked: %v", r)
		}
	}()
	if it, ok := tool.(tools.ImageTool); ok {
		return it.ExecuteWithImages(ctx, args)
	}
	text, err := tool.Execute(ctx, args)
	return tools.Result{Text: text}, err
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

// speed is the turn's generation speed in tokens per second: output tokens over the time spent
// generating them, first tokens excluded. It is 0 before any output.
func (s *turnStats) speed() float64 {
	if s.sumGenDuration <= 0 || s.sumCompletion <= 0 {
		return 0
	}
	return float64(s.sumCompletion) / s.sumGenDuration.Seconds()
}

// liveStatus is the turn so far for the status line while the model works: the step, the
// latest prompt's size, the output and its speed. It is empty before any step reports usage.
func (s *turnStats) liveStatus() string {
	var parts []string
	if s.totalSteps > 1 {
		parts = append(parts, fmt.Sprintf("step %d", s.totalSteps))
	}
	if s.hasUsage {
		parts = append(parts, FormatCompactNumber(s.lastPromptTokens)+" in", FormatCompactNumber(s.sumCompletion)+" out")
		if speed := s.speed(); speed > 0 {
			parts = append(parts, fmt.Sprintf("%.0f tokens/s", speed))
		}
	}
	return strings.Join(parts, " · ")
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
	if speed := s.speed(); speed > 0 {
		speedStr = fmt.Sprintf("%.1f tokens/s", speed)
	}

	// Total time and time to first token, then the local time the turn ended, so a turn
	// left running can be read on return as done and when.
	end := time.Now()
	timeStr := fmt.Sprintf("%s total", FormatDuration(end.Sub(s.turnStart)))
	if s.lastTTFT > 0 && s.totalSteps <= 1 {
		timeStr += fmt.Sprintf(" (%s to first token)", FormatDuration(s.lastTTFT))
	}
	timeStr += ", done at " + end.Format("15:04")

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

func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		// As the status line counts it: a 2h20m turn read as 8400.0s.
		return terminal.FormatElapsed(int(d.Round(time.Second).Seconds()))
	}
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

// FormatCompactNumber writes n for a narrow status line, rounded down: 950, 9.5K, 412K or 1.2M.
func FormatCompactNumber(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10_000:
		return fmt.Sprintf("%d.%dK", n/1000, n%1000/100)
	case n < 1_000_000:
		return fmt.Sprintf("%dK", n/1000)
	default:
		return fmt.Sprintf("%d.%dM", n/1_000_000, n%1_000_000/100_000)
	}
}

// ProtocolDetectedNotice says which protocol a provider was found to speak and whether the
// config file was updated so it is not probed again.
func ProtocolDetectedNotice(provider, api string, saved bool) string {
	if saved {
		return fmt.Sprintf("[provider %s speaks the %s API; saved \"api\": %q to the config file]", provider, api, api)
	}
	return fmt.Sprintf("[provider %s speaks the %s API; the config file could not be updated, so it will be probed again next run]", provider, api)
}
