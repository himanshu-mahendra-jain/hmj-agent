package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"hmj-agent/v2/internal/chat"
	"hmj-agent/v2/internal/logging"
	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/terminal"
	"hmj-agent/v2/internal/terminal/theme"
)

// Compaction is a request that compacts one conversation, running in the background from the end
// of a turn until Finish applies its result.
type Compaction struct {
	conv   *session.Conversation
	upTo   int // the number of messages compacted
	tokens int // the conversation's estimated size when compaction started
	cancel context.CancelFunc
	done   chan struct{}
	result chat.Message
	err    error
}

// StartCompaction starts compacting conv when it is over session.CompactAtTokens, or when
// turnErr, the error the turn ended with, is the provider's refusal of a request too long for the
// model, which the estimate may not foresee on a model with a smaller context window. It returns
// nil when neither holds or there is nothing to compact. It is called once a turn has ended; a
// turn compacts itself between its steps (compactMidTurn). The request is made with the
// conversation as it stands, on a client that reports only to the diagnostic log, so it can run
// while the next prompt is typed; nothing may be appended to conv until Finish has run.
func (r *Runner) StartCompaction(ctx context.Context, conv *session.Conversation, turnErr error) *Compaction {
	tooLong := chat.IsContextOverflow(turnErr) && conv.CanCompact()
	if !tooLong && !conv.NeedsCompaction() {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &Compaction{
		conv:   conv,
		upTo:   len(conv.Messages),
		tokens: conv.EstimateTokens(),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	messages := slices.Clone(conv.Messages)
	client := r.Client.Quiet()
	logging.L().Info("compacting conversation", "model", conv.Model.Model, "tokens", c.tokens, "after_too_long", tooLong)
	go func() {
		defer close(c.done)
		c.result, c.err = client.Compact(ctx, conv.Model, messages, r.ToolDefs)
	}()
	return c
}

// Finish waits for the compaction to end, under a spinner that Esc can stop, then puts the
// compacted conversation in place and reports the result. A failed or stopped compaction leaves
// the conversation as it was; it is tried again after the next turn. Finish on a nil Compaction
// does nothing.
func (c *Compaction) Finish() {
	if c == nil {
		return
	}
	defer c.cancel()
	select {
	case <-c.done:
	default:
		spinner := terminal.NewSpinner("compacting the conversation…")
		stopHaltListener := terminal.StartHaltListener(c.cancel)
		spinner.Start()
		<-c.done
		spinner.Stop()
		stopHaltListener()
	}

	err := c.err
	if err == nil {
		err = c.conv.ApplyCompaction(c.result, c.upTo)
	}
	var historyErr error
	if errors.Is(err, session.ErrHistoryNotSaved) {
		historyErr, err = err, nil
	}
	if err != nil {
		reportCompactionError(err)
		return
	}
	reportCompacted(c.conv, c.tokens, historyErr)
}

// reportCompacted tells the user conv was compacted from about before tokens, and warns when its
// replaced turns were not kept for /history (historyErr).
func reportCompacted(conv *session.Conversation, before int, historyErr error) {
	after := conv.EstimateTokens()
	logging.L().Info("conversation compacted", "model", conv.Model.Model, "tokens_before", before, "tokens_after", after)
	fmt.Println(theme.Muted(fmt.Sprintf("[compacted the conversation: ~%s → ~%s tokens]", FormatNumber(before), FormatNumber(after))))
	if historyErr != nil {
		logging.L().Warn("earlier turns not archived", "error", historyErr.Error())
		fmt.Println(theme.Caution(fmt.Sprintf("[%v]", historyErr)))
	}
}

// midTurnRetryTokens is how much a conversation must grow after a compaction failed during a turn
// before the turn tries again, so a provider that keeps failing is not asked at every step.
const midTurnRetryTokens = 100_000

// compactMidTurn compacts the first upTo messages of conv while a turn runs: past
// session.CompactAtTokens, or when the provider refused a step as too long for the model. The
// messages after upTo are kept, which only a turn that has no reply yet may ask for: its prompt.
// Once the model has replied, the compaction replaces every message after the system prompt, the
// turn's own too, since Claude accepts a thinking block only after an unchanged history, and
// session.ContinueAfterCompaction tells the model to go on. It runs under the turn's ctx, so a
// halt stops it. It reports whether conv was compacted; a failure is reported, and the turn goes
// on as it was.
func (r *Runner) compactMidTurn(ctx context.Context, conv *session.Conversation, spinner *terminal.Spinner, upTo int) bool {
	before := conv.EstimateTokens()
	wholeTurn := upTo == len(conv.Messages)
	logging.L().Info("compacting conversation mid-turn", "model", conv.Model.Model, "tokens", before)
	spinner.SetLabel("compacting the conversation…")
	spinner.SetDetail("")
	spinner.Start()
	compacted, err := r.Client.Compact(ctx, conv.Model, slices.Clone(conv.Messages[:upTo]), r.ToolDefs)
	spinner.Stop()
	if err == nil {
		err = conv.ApplyCompaction(compacted, upTo)
	}
	var historyErr error
	if errors.Is(err, session.ErrHistoryNotSaved) {
		historyErr, err = err, nil
	}
	if err != nil {
		if ctx.Err() == nil { // a halt is reported as the turn ends
			logging.L().Warn("conversation not compacted mid-turn", "error", err.Error())
			fmt.Println(theme.Caution(fmt.Sprintf("[could not compact the conversation: %s; the turn goes on]", theme.Sanitize(err.Error()))))
		}
		return false
	}
	if wholeTurn {
		conv.Messages = append(conv.Messages, chat.Message{Role: "user", Content: session.ContinueAfterCompaction})
	}
	reportCompacted(conv, before, historyErr)
	return true
}

// reportCompactionError tells the user the conversation was not compacted and why.
func reportCompactionError(err error) {
	if errors.Is(err, context.Canceled) {
		logging.L().Info("compaction stopped")
		fmt.Println(theme.Muted("[compaction stopped; it is tried again after the next turn]"))
		return
	}
	logging.L().Warn("conversation not compacted", "error", err.Error())
	fmt.Println(theme.Caution(fmt.Sprintf("[could not compact the conversation: %s; it is tried again after the next turn]", theme.Sanitize(err.Error()))))
}
