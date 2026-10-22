package agent

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/logging"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal/theme"
)

// titleInstruction is the system message of the request that names a conversation. The request
// carries no guidelines or tools, so the instruction is the whole brief.
const titleInstruction = "Write a 3–6 word title for this conversation. Reply with the title only: no quotes, no punctuation at the end, no markdown."

// Limits that keep the title request small and the title short.
const (
	titlePromptRunes     = 1000 // the first user prompt sent to the model
	titleReplyRunes      = 300  // the assistant's first reply sent to the model
	titleMaxRunes        = 60   // the longest title kept
	titleMaxOutputTokens = 40   // the output cap for the title request when reasoning is off
	// titleReasoningMaxOutputTokens is the output cap when the model reasons at low effort. The
	// reasoning counts against the cap, so 40 tokens would leave no room for the title.
	titleReasoningMaxOutputTokens = 256
)

// titleTimeout bounds the title request. The prompt returns only after it, and no spinner covers
// the wait, so a slow provider must not make the app look stuck. It is a variable so tests can
// shorten it.
var titleTimeout = 8 * time.Second

// errTitleToolCall is returned when the model answered the title request with a tool call.
var errTitleToolCall = errors.New("the model called a tool instead of writing a title")

// maybeGenerateTitle replaces the automatic name of a conversation with one the model writes from
// its first exchange. It runs once, after the first turn has finished normally, and only while the
// name is still automatic. It is silent: a failure, a cancelled turn or an empty title leaves the
// heuristic name in place.
func (r *Runner) maybeGenerateTitle(ctx context.Context, conv *session.Conversation, prompt, reply string) {
	if !conv.AutoTitled || conv.UserMessageCount() != 1 || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, titleTimeout)
	defer cancel()
	title, err := r.generateTitle(ctx, conv.Model, prompt, reply)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logging.L().Warn("conversation not titled", "error", err.Error())
		}
		return
	}
	if title == "" || !conv.AutoTitled {
		return
	}
	conv.Name = title
	conv.UpdatedAt = time.Now()
}

// generateTitle asks the model named by ref for a title for the exchange. The request is sent on a
// copy of the client that caps the output and keeps reasoning to a minimum (see titleSettings),
// with no retries, and it carries no tools and only the instruction and the truncated exchange,
// not the transcript.
func (r *Runner) generateTitle(ctx context.Context, ref config.ModelRef, prompt, reply string) (string, error) {
	request := []chat.Message{
		{Role: "system", Content: titleInstruction},
		{Role: "user", Content: titleExchange(prompt, reply)},
	}

	provider, _, err := r.Client.Config.ResolveModel(ref)
	if err != nil {
		return "", err
	}
	// The protocol is already known from the turn that just ran, so this sends no request.
	api, _, _ := r.Client.DetectWireAPI(ctx, ref.Provider, provider)
	client := r.Client.WithModelOverride(ref, func(m *config.Model) { titleSettings(api, m) })
	client.RetryConfig = chat.RetryConfig{}

	discard := func(string) {}
	msg, _, err := client.StreamChat(ctx, ref, request, nil, discard, discard)
	if err != nil {
		return "", err
	}
	if len(msg.ToolCalls) > 0 {
		return "", errTitleToolCall
	}
	return cleanTitle(msg.Content), nil
}

// titleSettings edits the model's settings for the title request on protocol api. Reasoning
// models of the Anthropic, Responses and Gemini protocols reason even when no reasoning parameters
// are sent (thinking cannot be turned off on the Claude 5 models), and that reasoning would use up
// a small cap, so they are asked for low effort and given room for it. A model configured with
// thinking off does not reason, and Chat Completions sends no reasoning parameters, so both keep
// the small cap.
func titleSettings(api string, m *config.Model) {
	m.MaxOutputTokens = titleMaxOutputTokens
	if !m.ThinkingEnabled() {
		return
	}
	if api != config.APICompletions {
		m.Effort = "low"
		m.MaxOutputTokens = titleReasoningMaxOutputTokens
	}
}

// titleExchange labels the first prompt and the first reply, each truncated, for the title request.
func titleExchange(prompt, reply string) string {
	var b strings.Builder
	b.WriteString("First message from the user:\n")
	b.WriteString(truncateRunes(strings.TrimSpace(prompt), titlePromptRunes))
	b.WriteString("\n\nStart of the assistant's reply:\n")
	b.WriteString(truncateRunes(strings.TrimSpace(reply), titleReplyRunes))
	return b.String()
}

// truncateRunes returns s cut to at most n runes, with an ellipsis when anything was cut.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// cleanTitle turns the model's reply into a title: the first non-empty line, without quotes,
// markdown markers, a "Title:" label or a trailing period, with its whitespace collapsed and its
// length capped. It returns "" when nothing is left.
func cleanTitle(reply string) string {
	reply = theme.Sanitize(reply)
	line := ""
	for l := range strings.SplitSeq(reply, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	line = trimTitleMarks(line)
	if len(line) >= len("title:") && strings.EqualFold(line[:len("title:")], "title:") {
		line = trimTitleMarks(line[len("title:"):])
	}
	line = strings.TrimRight(line, ".")
	line = strings.Join(strings.Fields(line), " ")
	runes := []rune(line)
	if len(runes) > titleMaxRunes {
		line = strings.TrimRightFunc(string(runes[:titleMaxRunes]), unicode.IsSpace)
	}
	return strings.TrimSpace(line)
}

// trimTitleMarks strips the quotes, backticks, asterisks, hashes and spaces around a title.
func trimTitleMarks(s string) string {
	return strings.Trim(s, "\"'`*#“”‘’ \t")
}
