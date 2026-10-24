package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/logging"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
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

// StartCompaction starts compacting conv when it is over session.CompactAtTokens, and returns nil
// when it is not. It is called once a turn has ended, never during one. The request is made with
// the conversation as it stands, on a client that reports only to the diagnostic log, so it can
// run while the next prompt is typed; nothing may be appended to conv until Finish has run.
func (r *Runner) StartCompaction(ctx context.Context, conv *session.Conversation) *Compaction {
	if !conv.NeedsCompaction() {
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
	logging.L().Info("compacting conversation", "model", conv.Model.Model, "tokens", c.tokens)
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
	after := c.conv.EstimateTokens()
	logging.L().Info("conversation compacted", "model", c.conv.Model.Model, "tokens_before", c.tokens, "tokens_after", after)
	fmt.Println(theme.Muted(fmt.Sprintf("[compacted the conversation: ~%s → ~%s tokens]", FormatNumber(c.tokens), FormatNumber(after))))
	if historyErr != nil {
		logging.L().Warn("earlier turns not archived", "error", historyErr.Error())
		fmt.Println(theme.Caution(fmt.Sprintf("[%v]", historyErr)))
	}
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
