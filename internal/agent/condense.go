package agent

import (
	"context"
	"errors"
	"fmt"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/logging"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
)

// summaryInstruction is the request that follows the older turns when the model is asked
// to summarize them. The summary takes those turns' place in later requests, so it asks for
// what the model will need to carry on, in the model's own voice.
const summaryInstruction = `Write a summary of the conversation above for your own use: in later requests this summary will stand in place of those messages, and you will continue the conversation from it. Write in the first person, as the assistant.

Include:
- what the user asked for, and why, and the preferences and instructions they gave;
- the decisions made and the reasons for them;
- what has been done so far, naming the files, commands, URLs and identifiers involved;
- what was found that later work depends on: facts, values, errors seen and how they were resolved;
- what remains to be done, and any open questions.

Leave out what a later turn will not need, and do not repeat tool output. Do not call any tool. Reply with the summary only.`

// errSummaryToolCall is returned when the model answered the summary request with a tool call.
var errSummaryToolCall = errors.New("the model called a tool instead of writing the summary")

// summarizer returns the Summarizer that asks conv's model to write the summary of the
// history it is given. The request carries the tool definitions, because providers reject
// a transcript with tool calls that defines no tools, and the instruction tells the model
// not to use them. The reply is not shown; a spinner covers the wait.
func (r *Runner) summarizer(conv *session.Conversation) session.Summarizer {
	return func(ctx context.Context, history []chat.Message) (string, error) {
		request := make([]chat.Message, 0, len(history)+1)
		request = append(request, history...)
		request = append(request, chat.Message{Role: "user", Content: summaryInstruction})

		spinner := terminal.NewSpinner("condensing earlier turns…")
		spinner.Start()
		defer spinner.Stop()

		discard := func(string) {}
		reply, _, err := r.Client.StreamChat(ctx, conv.Model, request, r.ToolDefs, discard, discard)
		if err != nil {
			return "", err
		}
		if len(reply.ToolCalls) > 0 {
			return "", errSummaryToolCall
		}
		return reply.Content, nil
	}
}

// condense replaces the older turns of a long conversation with a summary written by its
// model, so the transcript sent with each request stays short. A failure leaves the
// transcript as it was: the turn goes ahead and condensing is tried again next turn.
func (r *Runner) condense(ctx context.Context, conv *session.Conversation) {
	before, turns := conv.EstimateTokens(), conv.UserMessageCount()
	condensed, err := conv.Condense(ctx, session.DefaultKeepTurnsAfterSummary, r.summarizer(conv))
	if err != nil {
		reportCondenseError(err)
		return
	}
	if condensed {
		reportCondensed(conv, before, turns)
	}
}

// fitContext condenses conv when it is estimated to fill most of the model's configured
// context window, so the next request is not rejected as too long.
func (r *Runner) fitContext(ctx context.Context, conv *session.Conversation) {
	_, model, err := r.Client.Config.ResolveModel(conv.Model)
	if err != nil || model.ContextWindow <= 0 {
		return
	}
	before, turns := conv.EstimateTokens(), conv.UserMessageCount()
	budget := model.ContextWindow * contextBudgetPercent / 100
	if before <= budget {
		return
	}
	changed, err := conv.FitContext(ctx, budget, r.summarizer(conv))
	if err != nil {
		reportCondenseError(err)
	}
	if changed {
		fmt.Println(theme.Muted("[conversation near the model's context window]"))
		reportCondensed(conv, before, turns)
	}
}

// reportCondensed prints and logs what condensing achieved.
func reportCondensed(conv *session.Conversation, tokensBefore, turnsBefore int) {
	after := conv.EstimateTokens()
	condensedTurns := turnsBefore - conv.UserMessageCount() + 1 // the summary counts as a user message
	logging.L().Info("context condensed", "model", conv.Model.Model, "turns", condensedTurns, "tokens_before", tokensBefore, "tokens_after", after)
	fmt.Println(theme.Muted(fmt.Sprintf("[condensed %d earlier turns into a summary: ~%d → ~%d tokens]", condensedTurns, tokensBefore, after)))
}

// reportCondenseError tells the user condensing was skipped and why. A cancelled turn is
// reported by the halt itself, so nothing more is printed for it.
func reportCondenseError(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	logging.L().Warn("context not condensed", "error", err.Error())
	fmt.Println(theme.Caution(fmt.Sprintf("[could not condense earlier turns: %v]", err)))
}
