package chat

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"hmj-agent/internal/config"
	"hmj-agent/internal/logging"
)

// DefaultAnthropicMaxTokens is the output limit applied to Anthropic requests when the model does
// not set maxOutputTokens. Anthropic requires an explicit max_tokens field and recommends 64000
// for agentic work: a response cut off at the limit fails the step, and a higher limit costs
// nothing until it is used.
const DefaultAnthropicMaxTokens = 64000

// AnthropicVersion is the Messages API version sent in the anthropic-version header. The changes
// it brought over 2023-06-01 (no sampling parameters, no document blocks in assistant messages,
// no citations in user messages) concern nothing hmj sends.
const AnthropicVersion = "2026-10-21"

// anthropicCompactionBeta is the beta that on-demand compaction needs, on the compaction request
// and on every request that carries its block.
const anthropicCompactionBeta = "compact-2026-09-04"

type anthropicRequest struct {
	Model        string                 `json:"model"`
	System       []anthropicContent     `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	MaxTokens    int                    `json:"max_tokens"`
	Stream       bool                   `json:"stream"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	// Speed "fast" requests fast mode; omitted means standard speed.
	Speed string `json:"speed,omitempty"`
	// Compaction asks for a summary of the messages instead of a reply.
	Compaction *anthropicCompaction `json:"compaction,omitempty"`
}

// anthropicCompaction is the on-demand compaction parameter.
type anthropicCompaction struct {
	Type string `json:"type"`
}

// anthropicThinking enables adaptive thinking. Display "summarized" streams the reasoning so it
// can be shown live; budget_tokens is not sent because current models reject it.
type anthropicThinking struct {
	Type    string `json:"type"`
	Display string `json:"display"`
}

// anthropicOutputConfig carries the effort level that trades reasoning depth for speed.
type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitzero"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content is a tool result's output, as a string or, when it carries images, a list of
	// text and image blocks; or a compaction block's summary.
	Content any `json:"content,omitzero"`
	// Source is an image block's picture.
	Source *anthropicImageSource `json:"source,omitempty"`
	// IsError marks a tool_result whose tool failed.
	IsError bool `json:"is_error,omitzero"`
	// Thinking is a pointer so an empty thinking text is still sent.
	Thinking  *string `json:"thinking,omitzero"`
	Signature string  `json:"signature,omitempty"`
	Data      string  `json:"data,omitempty"`
	// CacheControl marks a prompt-caching breakpoint (Anthropic ephemeral cache).
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// anthropicToolResultContent is a tool result's content: its text, or with images, a list of
// a text block and image blocks. A picture that cannot be loaded becomes a text note.
func anthropicToolResultContent(msg Message) any {
	if len(msg.Images) == 0 {
		if msg.Content == "" {
			return nil
		}
		return msg.Content
	}
	var blocks []anthropicContent
	if msg.Content != "" {
		blocks = append(blocks, anthropicContent{Type: "text", Text: msg.Content})
	}
	for _, img := range msg.Images {
		data, note := img.base64Data()
		if note != "" {
			blocks = append(blocks, anthropicContent{Type: "text", Text: note})
			continue
		}
		blocks = append(blocks, anthropicContent{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: img.MediaType, Data: data}})
	}
	return blocks
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

var ephemeralCache = &anthropicCacheControl{Type: "ephemeral"}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema Schema `json:"input_schema"`
	// EagerInputStreaming sends the tool's input as the model writes it, without the server
	// buffering and validating each value first.
	EagerInputStreaming bool                   `json:"eager_input_streaming,omitzero"`
	CacheControl        *anthropicCacheControl `json:"cache_control,omitempty"`
}

// isThinkingBlock reports whether an Anthropic content block type is a reasoning block, which
// cannot carry a cache_control breakpoint.
func isThinkingBlock(blockType string) bool {
	return blockType == "thinking" || blockType == "redacted_thinking"
}

// convertMessagesToAnthropic converts the transcript into the Anthropic system prompt and message
// list. Assistant thinking blocks are replayed first, verbatim and in their original order, because
// the API requires them to precede the text and tool_use blocks of the same turn. A compaction
// block is replayed as the API returned it.
func convertMessagesToAnthropic(messages []Message) (system string, msgs []anthropicMessage) {
	appendBlock := func(role string, block anthropicContent) {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == role {
			msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, block)
			return
		}
		msgs = append(msgs, anthropicMessage{Role: role, Content: []anthropicContent{block}})
	}

	for _, msg := range portableCompactions(messages, BlockAnthropicCompaction) {
		switch msg.Role {
		case "system":
			if system != "" {
				system += "\n\n"
			}
			system += msg.Content
		case "user":
			appendBlock("user", anthropicContent{Type: "text", Text: msg.Content})
		case "tool":
			appendBlock("user", anthropicContent{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   anthropicToolResultContent(msg),
				IsError:   strings.HasPrefix(msg.Content, "error:"),
			})
		case "assistant":
			for _, b := range msg.Blocks {
				switch b.Type {
				case "thinking":
					text := b.Text
					appendBlock("assistant", anthropicContent{Type: "thinking", Thinking: &text, Signature: b.Signature})
				case "redacted_thinking":
					appendBlock("assistant", anthropicContent{Type: "redacted_thinking", Data: b.Data})
				case BlockAnthropicCompaction:
					appendBlock("assistant", anthropicContent{Type: BlockAnthropicCompaction, Content: b.Text, Signature: b.Signature})
				}
			}
			if msg.Content != "" {
				appendBlock("assistant", anthropicContent{Type: "text", Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				appendBlock("assistant", anthropicContent{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: anthropicToolInput(tc.Function.Arguments)})
			}
		}
	}
	return system, msgs
}

// anthropicToolInput converts a tool-call arguments string into the JSON
// object Anthropic requires for tool_use.input. Empty arguments mean a
// no-argument call ({}). Anything that fails to parse — or parses to a
// non-object — is preserved verbatim under a well-known key instead of being
// silently replaced with {}, so no argument data is ever dropped.
func anthropicToolInput(arguments string) map[string]any {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(arguments), &v); err != nil || v == nil {
		return map[string]any{"_malformed_arguments": arguments}
	}
	return v
}

// convertToolsToAnthropic advertises the tools without strict. A strict tool's arguments must be
// written in the order its schema lists them, with optional ones skipped but never moved, and
// Claude does not keep to one order: grep calls whose model meant to write output_mode and path
// first, and its long pattern last, came out as {"pattern": "x", "output_mode": ...}, the path and
// the real pattern lost, in 23 of 32 replays and 0 of 27 without strict. Each tool checks its own
// arguments, as it does for every provider.
func convertToolsToAnthropic(tools []ToolDefinition) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	res := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		}
		res = append(res, anthropicTool{
			Name:                t.Function.Name,
			Description:         t.Function.Description,
			InputSchema:         schema,
			EagerInputStreaming: t.StreamArguments,
		})
	}
	return res
}

// anthropicEvent holds every field the handlers read from a Messages stream event. Each event is
// decoded once into this struct, so fields that only some event types carry stay empty otherwise.
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
		// Content is a compaction block's summary. Other block types carry content of other
		// shapes, so it is decoded loosely.
		Content any `json:"content"`
	} `json:"content_block"`
	Delta struct {
		Type        string                `json:"type"`
		Text        string                `json:"text"`
		Thinking    string                `json:"thinking"`
		Signature   string                `json:"signature"`
		PartialJSON string                `json:"partial_json"`
		StopReason  *string               `json:"stop_reason"`
		StopDetails *anthropicStopDetails `json:"stop_details"`
	} `json:"delta"`
	// Usage is the output-token count carried by message_delta.
	Usage anthropicUsage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	// Speed is the speed the request ran at, "fast" or "standard", when the API reports it.
	Speed string `json:"speed"`
	// Iterations reports each sampling call of the request. A compaction request reports its
	// summarization call here, with zero top-level counts.
	Iterations []anthropicUsage `json:"iterations"`
}

// anthropicStopDetails explains a stop_reason such as "refusal": the safety category and a short
// explanation, when the API provides them.
type anthropicStopDetails struct {
	Type        string `json:"type"`
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

// anthropicBlockAcc accumulates one thinking or redacted_thinking block streamed in content order.
type anthropicBlockAcc struct {
	block Block
	text  strings.Builder
}

type anthropicAccumulator struct {
	startTime time.Time
	// toolProgress is told about each tool call as the model writes it.
	toolProgress toolProgress
	// firstToken is when output was first shown, which decides whether a retry would repeat it.
	// firstOutput is when the first content block began, including a tool call or thinking that
	// shows no text; it times the response.
	firstToken       time.Time
	firstOutput      time.Time
	contentBuilder   strings.Builder
	toolCallsByIndex map[int]*ToolCall
	toolArgsByIndex  map[int]*strings.Builder
	toolCallIndices  []int
	// reasoning holds thinking blocks in the order the model produced them, and reasoningByIndex
	// locates the block that a delta with a given content index belongs to.
	reasoning        []*anthropicBlockAcc
	reasoningByIndex map[int]*anthropicBlockAcc
	// compaction is the block a compaction request returned, whole in its content_block_start.
	compaction   *Block
	finishReason string
	refusal      *anthropicStopDetails
	usage        *TokenUsage
}

func newAnthropicAccumulator(startTime time.Time) *anthropicAccumulator {
	return &anthropicAccumulator{
		startTime:        startTime,
		toolCallsByIndex: make(map[int]*ToolCall),
		toolArgsByIndex:  make(map[int]*strings.Builder),
		reasoningByIndex: make(map[int]*anthropicBlockAcc),
		usage:            &TokenUsage{},
	}
}

// handle decodes one server-sent event once and dispatches it by type.
func (a *anthropicAccumulator) handle(eventType, data string, onReasoning, onDelta func(string)) error {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" {
		return nil
	}

	var ev anthropicEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		// Non-empty stream data that is not JSON is a protocol violation, not
		// an ignorable event; surface it like the responses handler does.
		snippet := trimmed
		count := 0
		for i := range snippet {
			if count == 200 {
				snippet = snippet[:i] + "..."
				break
			}
			count++
		}
		return fmt.Errorf("anthropic stream: invalid event JSON: %w (snippet: %s)", err, snippet)
	}

	evType := eventType
	if evType == "" {
		evType = ev.Type
	}

	switch evType {
	case "message_start":
		a.handleMessageStart(&ev)
	case "content_block_start":
		a.markOutput()
		a.handleContentBlockStart(&ev)
	case "content_block_delta":
		a.markOutput()
		a.handleContentBlockDelta(&ev, onReasoning, onDelta)
	case "message_delta":
		a.handleMessageDelta(&ev)
	case "error":
		return a.handleErrorEvent(&ev, trimmed)
	}

	return nil
}

// markOutput records when the response's first content block began, once.
func (a *anthropicAccumulator) markOutput() {
	if a.firstOutput.IsZero() {
		a.firstOutput = time.Now()
	}
}

// handleMessageStart records the input-side token usage and cache counters.
func (a *anthropicAccumulator) handleMessageStart(ev *anthropicEvent) {
	u := ev.Message.Usage
	// Anthropic reports cache reads and cache writes apart from input_tokens. The prompt total
	// includes them, matching the prompt_tokens the other protocols report.
	a.usage.InputTokens = u.InputTokens
	a.usage.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if u.Speed != "" {
		a.usage.Speed = u.Speed
	}
	if u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0 {
		a.usage.CachedTokens = u.CacheReadInputTokens
		a.usage.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens:         u.CacheReadInputTokens,
			CacheReadInputTokens: u.CacheReadInputTokens,
		}
	}
}

// handleContentBlockStart registers tool_use blocks so their arguments can be collected, and
// thinking blocks so their text and signature can be replayed.
func (a *anthropicAccumulator) handleContentBlockStart(ev *anthropicEvent) {
	switch ev.ContentBlock.Type {
	case "tool_use":
		tc := &ToolCall{
			ID:   ev.ContentBlock.ID,
			Type: "function",
			Function: ToolCallFunction{
				Name: ev.ContentBlock.Name,
			},
		}
		a.toolCallsByIndex[ev.Index] = tc
		a.toolArgsByIndex[ev.Index] = &strings.Builder{}
		a.toolCallIndices = append(a.toolCallIndices, ev.Index)
		a.toolProgress.report(tc.Function.Name, 0)
	case "thinking", "redacted_thinking":
		acc := &anthropicBlockAcc{block: Block{
			Type:      ev.ContentBlock.Type,
			Signature: ev.ContentBlock.Signature,
			Data:      ev.ContentBlock.Data,
		}}
		a.reasoning = append(a.reasoning, acc)
		a.reasoningByIndex[ev.Index] = acc
	case BlockAnthropicCompaction:
		summary, _ := ev.ContentBlock.Content.(string)
		a.compaction = &Block{Type: BlockAnthropicCompaction, Text: summary, Signature: ev.ContentBlock.Signature}
	}
}

// handleContentBlockDelta streams text and reasoning deltas to the callbacks, records thinking
// text and signatures for replay, and accumulates tool argument fragments.
func (a *anthropicAccumulator) handleContentBlockDelta(ev *anthropicEvent, onReasoning, onDelta func(string)) {
	switch ev.Delta.Type {
	case "text_delta":
		if ev.Delta.Text != "" {
			if a.firstToken.IsZero() {
				a.firstToken = time.Now()
			}
			a.contentBuilder.WriteString(ev.Delta.Text)
			if onDelta != nil {
				onDelta(ev.Delta.Text)
			}
		}
	case "thinking_delta":
		if acc, ok := a.reasoningByIndex[ev.Index]; ok {
			acc.text.WriteString(ev.Delta.Thinking)
		}
		if ev.Delta.Thinking != "" {
			// Shown reasoning is delivered output: a retry would show it a second time.
			if a.firstToken.IsZero() {
				a.firstToken = time.Now()
			}
			if onReasoning != nil {
				onReasoning(ev.Delta.Thinking)
			}
		}
	case "signature_delta":
		if acc, ok := a.reasoningByIndex[ev.Index]; ok {
			acc.block.Signature += ev.Delta.Signature
		}
	case "input_json_delta":
		if b, ok := a.toolArgsByIndex[ev.Index]; ok {
			b.WriteString(ev.Delta.PartialJSON)
			a.toolProgress.report(a.toolCallsByIndex[ev.Index].Function.Name, b.Len())
		}
	}
}

// handleMessageDelta maps the stop reason to the OpenAI-style finish reason, keeps refusal details
// and records output token usage and the speed used.
func (a *anthropicAccumulator) handleMessageDelta(ev *anthropicEvent) {
	if ev.Delta.StopReason != nil {
		switch *ev.Delta.StopReason {
		case "tool_use":
			a.finishReason = "tool_calls"
		case "end_turn", "stop_sequence":
			a.finishReason = "stop"
		case "max_tokens":
			a.finishReason = "length"
		default:
			a.finishReason = *ev.Delta.StopReason
		}
	}
	if ev.Delta.StopDetails != nil {
		a.refusal = ev.Delta.StopDetails
	}
	if ev.Usage.OutputTokens > 0 {
		a.usage.OutputTokens = ev.Usage.OutputTokens
		a.usage.CompletionTokens = ev.Usage.OutputTokens
	}
	// A compaction request reports its tokens only per iteration.
	if a.usage.PromptTokens == 0 && a.usage.CompletionTokens == 0 {
		for _, it := range ev.Usage.Iterations {
			a.usage.PromptTokens += it.InputTokens + it.CacheReadInputTokens + it.CacheCreationInputTokens
			a.usage.InputTokens += it.InputTokens
			a.usage.CachedTokens += it.CacheReadInputTokens
			a.usage.OutputTokens += it.OutputTokens
			a.usage.CompletionTokens += it.OutputTokens
		}
	}
	if ev.Usage.Speed != "" {
		a.usage.Speed = ev.Usage.Speed
	}
}

// handleErrorEvent turns an Anthropic "error" event into a Go error. The error types whose HTTP
// statuses are retried (rate limit, server error, timeout, overload) are wrapped as
// retryableStreamError so the retry loop can try again before output starts.
func (a *anthropicAccumulator) handleErrorEvent(ev *anthropicEvent, trimmed string) error {
	if ev.Error.Type == "" && ev.Error.Message == "" {
		return fmt.Errorf("anthropic stream error: %s", truncateErrorBody([]byte(trimmed), maxShownErrorRunes))
	}
	err := fmt.Errorf("anthropic error (%s): %s", ev.Error.Type, ev.Error.Message)
	switch ev.Error.Type {
	case "rate_limit_error", "api_error", "timeout_error", "overloaded_error":
		return &retryableStreamError{err: err}
	}
	return err
}

// anthropicEffort maps an effort level onto the ones Anthropic takes (low to max): OpenAI's none
// and minimal become low.
func anthropicEffort(effort string) string {
	switch effort {
	case "none", "minimal":
		return "low"
	}
	return effort
}

// errIncompleteStream reports an Anthropic stream that ended without a stop reason: the
// connection closed cleanly before the response was complete, so its last content block, such as
// a tool call's arguments, may be cut short.
var errIncompleteStream = errors.New("anthropic stream ended before the response was complete")

func (a *anthropicAccumulator) result() (Message, *TokenUsage) {
	totalDuration := time.Since(a.startTime)
	var ttft time.Duration
	if !a.firstOutput.IsZero() {
		ttft = a.firstOutput.Sub(a.startTime)
	}

	usage := a.usage
	if usage == nil {
		usage = &TokenUsage{}
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	usage.Duration = totalDuration
	usage.TTFT = ttft
	usage.FinishReason = a.finishReason
	if a.refusal != nil {
		usage.RefusalCategory = a.refusal.Category
	}
	if usage.CompletionTokens > 0 && !a.firstOutput.IsZero() {
		genSec := time.Since(a.firstOutput).Seconds()
		if genSec > 0 {
			usage.TokensPerSecond = float64(usage.CompletionTokens) / genSec
		}
	}

	slices.Sort(a.toolCallIndices)
	toolCalls := make([]ToolCall, 0, len(a.toolCallIndices))
	for _, idx := range a.toolCallIndices {
		tc := a.toolCallsByIndex[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("toolu_%d", idx+1)
		}
		if b, ok := a.toolArgsByIndex[idx]; ok && b.Len() > 0 {
			tc.Function.Arguments = b.String()
		}
		toolCalls = append(toolCalls, *tc)
	}

	var blocks []Block
	for _, acc := range a.reasoning {
		acc.block.Text = acc.text.String()
		blocks = append(blocks, acc.block)
	}
	if a.compaction != nil {
		blocks = append(blocks, *a.compaction)
	}

	return Message{
		Role:      "assistant",
		Content:   a.contentBuilder.String(),
		ToolCalls: toolCalls,
		Blocks:    blocks,
	}, usage
}

// anthropicRequestFor builds the Messages API payload for one turn.
func anthropicRequestFor(model config.Model, messages []Message, tools []ToolDefinition) anthropicRequest {
	systemPrompt, anthropicMsgs := convertMessagesToAnthropic(messages)
	maxTokens := DefaultAnthropicMaxTokens
	if model.MaxOutputTokens > 0 {
		maxTokens = model.MaxOutputTokens
	}
	reqPayload := anthropicRequest{
		Model:     model.ID,
		Messages:  anthropicMsgs,
		MaxTokens: maxTokens,
		Stream:    true,
		Tools:     convertToolsToAnthropic(tools),
		Speed:     model.Speed,
	}
	if model.ThinkingEnabled() {
		reqPayload.Thinking = &anthropicThinking{Type: "adaptive", Display: "summarized"}
	}
	if model.Effort != "" {
		reqPayload.OutputConfig = &anthropicOutputConfig{Effort: anthropicEffort(model.Effort)}
	}
	// Prompt caching: one breakpoint at the end of the system prompt, one at
	// the end of the tool list, and one at the end of the conversation. The
	// stable prefix (system + tools + history) is then served from cache on
	// subsequent turns and only the new tail is billed at full rate.
	if systemPrompt != "" {
		reqPayload.System = []anthropicContent{{
			Type:         "text",
			Text:         systemPrompt,
			CacheControl: ephemeralCache,
		}}
	}
	if n := len(reqPayload.Tools); n > 0 {
		reqPayload.Tools[n-1].CacheControl = ephemeralCache
	}
	if n := len(reqPayload.Messages); n > 0 {
		last := &reqPayload.Messages[n-1]
		// Thinking blocks cannot carry cache_control, so the breakpoint goes on the
		// last block that is not reasoning.
		for i := len(last.Content) - 1; i >= 0; i-- {
			if !isThinkingBlock(last.Content[i].Type) {
				last.Content[i].CacheControl = ephemeralCache
				break
			}
		}
	}
	return reqPayload
}

// anthropicMessagesURL returns the Messages endpoint for baseURL, accepting a base URL
// that already ends in the messages path.
func anthropicMessagesURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/messages") || strings.HasSuffix(baseURL, "/v1/messages") {
		return baseURL
	}
	return baseURL + "/v1/messages"
}

// streamAnthropicOnce executes one streaming request against the Anthropic
// Messages API (POST /v1/messages).
func (c *Client) streamAnthropicOnce(
	ctx context.Context,
	provider config.Provider,
	model config.Model,
	messages []Message,
	tools []ToolDefinition,
	onReasoning func(string),
	onDelta func(string),
) (Message, *TokenUsage, time.Duration, bool, error) {
	return c.sendAnthropic(ctx, provider, anthropicRequestFor(model, messages, tools), onReasoning, onDelta)
}

// sendAnthropic sends one streaming Messages API request with payload as its body.
func (c *Client) sendAnthropic(
	ctx context.Context,
	provider config.Provider,
	payload anthropicRequest,
	onReasoning func(string),
	onDelta func(string),
) (Message, *TokenUsage, time.Duration, bool, error) {
	body, err := encodeRequestBody(payload)
	if err != nil {
		return Message{}, nil, 0, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL(provider.BaseURL), bytes.NewReader(body))
	if err != nil {
		return Message{}, nil, 0, false, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", AnthropicVersion)
	if payload.Compaction != nil || carriesCompaction(payload.Messages) {
		req.Header.Set("anthropic-beta", anthropicCompactionBeta)
	}
	if key := config.ResolveAPIKey(provider.APIKey); key != "" {
		req.Header.Set("x-api-key", key)
	}

	startTime := time.Now()
	acc := newAnthropicAccumulator(startTime)
	acc.toolProgress = toolProgressFrom(ctx)
	retryAfter, retryable, err := c.doStream(ctx, req, func(eventType, data string) error {
		return acc.handle(eventType, data, onReasoning, onDelta)
	}, func() bool { return !acc.firstToken.IsZero() })
	if err != nil {
		return Message{}, nil, retryAfter, retryable, err
	}
	// Every complete response carries a stop reason in message_delta. Without one the reply is
	// partial and must not be used; it is retried when no output has been shown yet.
	if acc.finishReason == "" {
		return Message{}, nil, 0, acc.firstToken.IsZero(), errIncompleteStream
	}

	msg, usage := acc.result()
	return msg, usage, 0, false, nil
}

// carriesCompaction reports whether msgs hold a compaction block.
func carriesCompaction(msgs []anthropicMessage) bool {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == BlockAnthropicCompaction {
				return true
			}
		}
	}
	return false
}

// compactAnthropic asks the Messages API to compact messages on demand (beta
// compact-2026-09-04): the request is the conversation's own request, with the same system
// prompt, tools, thinking, effort and speed, so it reads the conversation's prompt cache, plus
// the compaction parameter. The reply is one signed compaction block.
func (c *Client) compactAnthropic(ctx context.Context, provider config.Provider, model config.Model, messages []Message, tools []ToolDefinition) (Message, error) {
	payload := anthropicRequestFor(model, messages, tools)
	payload.Compaction = &anthropicCompaction{Type: "summarize"}
	discard := func(string) {}
	var reply Message
	var usage *TokenUsage
	err := c.withRetries(ctx, func() (time.Duration, bool, error) {
		var retryAfter time.Duration
		var retryable bool
		var err error
		reply, usage, retryAfter, retryable, err = c.sendAnthropic(ctx, provider, payload, discard, discard)
		return retryAfter, retryable, err
	})
	if err != nil {
		return Message{}, err
	}
	logging.L().Info("compaction request", "stop_reason", usage.FinishReason, "prompt_tokens", usage.PromptTokens,
		"cached_tokens", usage.CachedTokens, "output_tokens", usage.CompletionTokens)
	for _, b := range reply.Blocks {
		if b.Type == BlockAnthropicCompaction && usage.FinishReason == "compaction" {
			return Message{Role: "assistant", Blocks: []Block{b}}, nil
		}
	}
	return Message{}, noSummaryError(usage)
}

// noSummaryError explains a compaction request that returned no summary, from the stop reason of
// its summarization call.
func noSummaryError(usage *TokenUsage) error {
	switch usage.FinishReason {
	case "length":
		return errors.New("no summary came back: it was cut off at the output limit")
	case "tool_calls":
		return errSummaryToolCall
	case "refusal":
		if usage.RefusalCategory != "" {
			return fmt.Errorf("no summary came back: the model declined (safety classifier: %s)", usage.RefusalCategory)
		}
		return errors.New("no summary came back: the model declined")
	case "stop", "":
		return ErrEmptySummary
	default:
		return fmt.Errorf("no summary came back (stop reason %s)", usage.FinishReason)
	}
}
