package chat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/logging"
)

// The kinds of compaction block. A compaction message is an assistant message whose only content
// is one such block; it stands in place of the conversation it summarizes, right after the system
// prompt.
const (
	// BlockAnthropicCompaction is the signed block the Anthropic API returns for a compaction
	// request: Text holds the summary and Signature its signature. It is sent back unchanged.
	BlockAnthropicCompaction = "compaction"
	// BlockResponsesCompaction is the window the OpenAI Responses compact endpoint returns: Data
	// holds its output items as a JSON array, sent back as-is at the start of the input.
	BlockResponsesCompaction = "responses_compacted_window"
	// BlockSummary is a summary the model wrote in an ordinary request, for providers that have
	// no compaction API: Text holds it.
	BlockSummary = "summary"
)

// SummaryPrefix opens the user message that carries a summary to a provider that cannot replay
// the compaction block itself.
const SummaryPrefix = "[Summary of the earlier part of this conversation, written by the assistant for its own reference]"

// SummaryAck is the assistant reply that follows a summary, so the transcript keeps alternating
// roles.
const SummaryAck = "Understood. I will continue from this summary."

// unreadableSummary stands in for a compaction only the provider that made it can read.
const unreadableSummary = "The earlier part of this conversation was compacted by another provider, and its summary cannot be read here."

// summaryInstruction is the request that follows the conversation when the model is asked to
// summarize it. The summary takes the conversation's place in later requests, so it asks for what
// the model will need to carry on, in the model's own voice.
const summaryInstruction = `Write a summary of the conversation above for your own use: in later requests this summary will stand in place of those messages, and you will continue the conversation from it. Write in the first person, as the assistant.

Include:
- what the user asked for, and why, and the preferences and instructions they gave;
- the decisions made and the reasons for them;
- what has been done so far, naming the files, commands, URLs and identifiers involved;
- what was found that later work depends on: facts, values, errors seen and how they were resolved;
- what remains to be done, and any open questions.

Leave out what a later turn will not need, and do not repeat tool output. Do not call any tool. Reply with the summary only.`

// ErrEmptySummary is returned when the model wrote no summary.
var ErrEmptySummary = errors.New("the model returned an empty summary")

// errSummaryToolCall is returned when the model answered the summary request with a tool call.
var errSummaryToolCall = errors.New("the model called a tool instead of writing the summary")

// CompactionBlock returns the compaction block of m, or nil when m is not a compaction message.
func (m *Message) CompactionBlock() *Block {
	if m.Role != "assistant" || len(m.Blocks) != 1 {
		return nil
	}
	switch m.Blocks[0].Type {
	case BlockAnthropicCompaction, BlockResponsesCompaction, BlockSummary:
		return &m.Blocks[0]
	}
	return nil
}

// ReadableSummary returns the summary text of a compaction message, or "" when the provider's
// block cannot be read.
func (b *Block) ReadableSummary() string {
	if b.Type == BlockResponsesCompaction {
		return ""
	}
	return b.Text
}

// portableCompactions returns messages with every compaction message whose block is not of kind
// native replaced by the summary in plain text: a user message that carries it, then SummaryAck.
// Every protocol can send that, so a conversation compacted under one provider continues under
// another. messages is returned unchanged when there is nothing to replace.
func portableCompactions(messages []Message, native string) []Message {
	var out []Message
	for i := range messages {
		b := messages[i].CompactionBlock()
		if b == nil || b.Type == native {
			if out != nil {
				out = append(out, messages[i])
			}
			continue
		}
		if out == nil {
			out = append(make([]Message, 0, len(messages)+1), messages[:i]...)
		}
		summary := b.ReadableSummary()
		if summary == "" {
			summary = unreadableSummary
		}
		out = append(out,
			Message{Role: "user", Content: SummaryPrefix + "\n" + summary},
			Message{Role: "assistant", Content: SummaryAck},
		)
	}
	if out == nil {
		return messages
	}
	return out
}

// Compact asks the model named by ref to compact messages, the conversation so far led by its
// system prompt, and returns the compaction message that replaces everything after the system
// prompt. tools are the conversation's tool definitions, which the request carries as the
// conversation's requests do. The provider's compaction API is used where it has one: Anthropic's
// on-demand compaction, or the OpenAI Responses compact endpoint. Elsewhere the model writes a summary in an ordinary request, which is
// also the fallback when the provider rejects the compaction request as invalid or does not
// offer it, as a compatible service or an older model may. When the provider finds the
// conversation too long for the model, the model summarizes a shortened copy of it instead
// (summarizeShortened). Retryable failures are retried according to RetryConfig.
func (c *Client) Compact(ctx context.Context, ref config.ModelRef, messages []Message, tools []ToolDefinition) (Message, error) {
	provider, model, err := c.Config.ResolveModel(ref)
	if err != nil {
		return Message{}, err
	}
	// As for a turn, only a cancelled detection stops the request (see streamChat).
	api, _, err := c.DetectWireAPI(ctx, ref.Provider, provider)
	if err != nil && ctx.Err() != nil {
		return Message{}, err
	}
	var compacted Message
	switch api {
	case config.APIAnthropic:
		compacted, err = c.compactAnthropic(ctx, provider, model, messages, tools)
	case config.APIResponses:
		compacted, err = c.compactResponses(ctx, provider, model, messages)
	default:
		return c.summarizeOrShorten(ctx, ref, messages, tools)
	}
	if IsContextOverflow(err) {
		logging.L().Warn("conversation too long to compact; summarizing a shortened copy", "provider", ref.Provider, "api", api, "error", err.Error())
		return c.summarizeShortened(ctx, ref, messages)
	}
	if statusErr, ok := errors.AsType[*StatusError](err); ok && (statusErr.Code == http.StatusBadRequest || statusErr.Code == http.StatusNotFound) {
		logging.L().Warn("compaction request rejected; summarizing instead", "provider", ref.Provider, "api", api, "error", err.Error())
		return c.summarizeOrShorten(ctx, ref, messages, tools)
	}
	return compacted, err
}

// summarizeOrShorten has the model summarize messages, and a shortened copy of them when they are
// too long for the model.
func (c *Client) summarizeOrShorten(ctx context.Context, ref config.ModelRef, messages []Message, tools []ToolDefinition) (Message, error) {
	compacted, err := c.summarize(ctx, ref, messages, tools)
	if IsContextOverflow(err) {
		logging.L().Warn("conversation too long to summarize; summarizing a shortened copy", "provider", ref.Provider, "error", err.Error())
		return c.summarizeShortened(ctx, ref, messages)
	}
	return compacted, err
}

// summarize compacts messages for a provider without a compaction API: the model writes a summary
// of them in an ordinary request that ends with summaryInstruction. The request carries the tool
// definitions, because providers reject a transcript with tool calls that defines no tools.
func (c *Client) summarize(ctx context.Context, ref config.ModelRef, messages []Message, tools []ToolDefinition) (Message, error) {
	request := make([]Message, 0, len(messages)+1)
	request = append(request, messages...)
	request = append(request, Message{Role: "user", Content: summaryInstruction})
	return c.writeSummary(ctx, ref, request, tools)
}

// writeSummary sends request, which ends by asking for a summary, and returns the reply as a
// summary compaction.
func (c *Client) writeSummary(ctx context.Context, ref config.ModelRef, request []Message, tools []ToolDefinition) (Message, error) {
	discard := func(string) {}
	reply, _, err := c.StreamChat(ctx, ref, request, tools, discard, discard)
	if err != nil {
		return Message{}, err
	}
	if len(reply.ToolCalls) > 0 {
		return Message{}, errSummaryToolCall
	}
	summary := strings.TrimSpace(reply.Content)
	if summary == "" {
		return Message{}, ErrEmptySummary
	}
	return Message{Role: "assistant", Blocks: []Block{{Type: BlockSummary, Text: summary}}}, nil
}

// withRetries runs attempt until it succeeds, fails with an error that is not retryable, or
// RetryConfig's retries are used up, waiting between attempts as RetryConfig and any wait the
// provider asked for say.
func (c *Client) withRetries(ctx context.Context, attempt func() (time.Duration, bool, error)) error {
	retryCfg := c.RetryConfig
	if retryCfg.MaxRetries < 0 {
		retryCfg = DefaultRetryConfig()
	}
	for n := 0; ; n++ {
		retryAfter, retryable, err := attempt()
		if err == nil {
			return nil
		}
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return err
		}
		if !retryable || n >= retryCfg.MaxRetries {
			return err
		}
		if err := c.waitToRetry(ctx, retryCfg, n, retryAfter, err); err != nil {
			return err
		}
	}
}
