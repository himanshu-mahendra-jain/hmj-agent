package chat

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"hmj-agent/internal/config"
)

type InputItem struct {
	Type      string `json:"type"`                // "message", "function_call", "function_call_output", "reasoning"
	Role      string `json:"role,omitempty"`      // "user", "assistant", "system", "developer"
	Content   string `json:"content,omitempty"`   // text content
	CallID    string `json:"call_id,omitempty"`   // function call id
	Name      string `json:"name,omitempty"`      // function name
	Arguments string `json:"arguments,omitempty"` // function arguments
	Output    any    `json:"output,omitzero"`     // tool output: a string, or with images []responsesOutputPart
	// Phase labels an assistant message "commentary" or "final_answer", as the model did.
	Phase string `json:"phase,omitempty"`
	// ID, EncryptedContent and Summary replay a reasoning item. Summary is a pointer so an
	// empty summary is still sent as [].
	ID               string    `json:"id,omitempty"`
	EncryptedContent string    `json:"encrypted_content,omitempty"`
	Summary          *[]string `json:"summary,omitzero"`
	// Raw, when set, is an item of a compacted window, sent exactly as the compact endpoint
	// returned it in place of the fields above.
	Raw jsontext.Value `json:"-"`
}

// BlockResponsesMessage is one message item of a Responses API reply that the model labelled with
// a phase: Text holds its text and Signature its phase ("commentary" or "final_answer"). The
// message's Content still holds all its text; these let the items go back as they came, phase
// and all, which OpenAI says later models need.
const BlockResponsesMessage = "responses_message"

// responsesOutputPart is one part of a function_call_output that carries images.
type responsesOutputPart struct {
	Type     string `json:"type"` // "input_text" or "input_image"
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // a data: URL
}

// responsesToolOutput is a tool result's output: its text, or with images, a list of a text
// part and image parts. A picture that cannot be loaded becomes a text note.
func responsesToolOutput(msg Message) any {
	if len(msg.Images) == 0 {
		return msg.Content
	}
	var parts []responsesOutputPart
	if msg.Content != "" {
		parts = append(parts, responsesOutputPart{Type: "input_text", Text: msg.Content})
	}
	for _, img := range msg.Images {
		data, note := img.base64Data()
		if note != "" {
			parts = append(parts, responsesOutputPart{Type: "input_text", Text: note})
			continue
		}
		parts = append(parts, responsesOutputPart{Type: "input_image", ImageURL: "data:" + img.MediaType + ";base64," + data})
	}
	return parts
}

// inputItemFields is InputItem without its MarshalJSONTo method.
type inputItemFields InputItem

// MarshalJSONTo writes Raw as it is, or else the item's fields.
func (it InputItem) MarshalJSONTo(enc *jsontext.Encoder) error {
	if it.Raw != nil {
		return enc.WriteValue(it.Raw)
	}
	return json.MarshalEncode(enc, inputItemFields(it), enc.Options())
}

// ResponsesRequest is the body of a POST /responses request.
type ResponsesRequest struct {
	Model  string          `json:"model"`
	Input  []InputItem     `json:"input"`
	Stream bool            `json:"stream"`
	Tools  []responsesTool `json:"tools,omitempty"`
	// Store is always false: hmj sends the whole conversation each turn and never reads a stored
	// response back, which OpenAI would otherwise keep for 30 days. Reasoning then comes back as
	// encrypted content, replayed in later turns.
	Store     bool                `json:"store"`
	Include   []string            `json:"include,omitempty"`
	Reasoning *responsesReasoning `json:"reasoning,omitempty"`
	// MaxOutputTokens caps the generated tokens; it is sent only when the model sets maxOutputTokens.
	MaxOutputTokens int `json:"max_output_tokens,omitzero"`
}

// responsesTool is a function tool as the Responses API takes it: its name, description and
// parameters beside its type, not under "function" as Chat Completions nests them.
type responsesTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  Schema `json:"parameters,omitempty"`
	// Strict is sent as false: left out, the API makes a schema strict where it can, which
	// makes every property required, and the tools' optional arguments would stop being so.
	Strict bool `json:"strict"`
}

// responsesTools converts tool definitions to the Responses API's shape.
func responsesTools(tools []ToolDefinition) []responsesTool {
	out := make([]responsesTool, len(tools))
	for i, t := range tools {
		out[i] = responsesTool{Type: "function", Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters}
	}
	return out
}

// responsesReasoning configures reasoning effort and the streamed reasoning summary.
type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary"`
}

type responsesStreamEvent struct {
	Type         string `json:"type"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	ItemID       string `json:"item_id"`
	CallID       string `json:"call_id"`
	Delta        string `json:"delta"`
	// Code and Message describe an "error" event.
	Code    string `json:"code"`
	Message string `json:"message"`

	Item *struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		Role             string `json:"role"`
		EncryptedContent string `json:"encrypted_content"`
		Summary          []struct {
			Text string `json:"text"`
		} `json:"summary"`
		// Phase and Content are a message item's label and its parts.
		Phase   string `json:"phase"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item,omitempty"`

	Arguments string `json:"arguments,omitempty"`

	Response *struct {
		ID     string      `json:"id"`
		Status string      `json:"status"`
		Usage  *TokenUsage `json:"usage"`
		// IncompleteDetails explains a "response.incomplete" stream end,
		// e.g. reason "max_output_tokens".
		IncompleteDetails *responsesIncompleteDetails `json:"incomplete_details,omitempty"`
		// Error explains a "response.failed" stream end.
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	} `json:"response,omitempty"`

	Usage *TokenUsage `json:"usage,omitempty"`
}

// responsesIncompleteDetails explains why a Responses API stream ended incomplete.
type responsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

// dropReasoningItems removes the reasoning items from items. They are sent only when reasoning is
// requested, since the server cannot resolve a reasoning item it did not store.
func dropReasoningItems(items []InputItem) []InputItem {
	return slices.DeleteFunc(items, func(it InputItem) bool { return it.Type == "reasoning" })
}

func convertMessagesToInputItems(messages []Message) []InputItem {
	messages = portableCompactions(messages, BlockResponsesCompaction)
	items := make([]InputItem, 0, len(messages))
	for _, msg := range messages {
		if b := msg.CompactionBlock(); b != nil {
			items = append(items, compactedWindowItems(b.Data)...)
			continue
		}
		switch msg.Role {
		case "tool":
			items = append(items, InputItem{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: responsesToolOutput(msg),
			})
		case "assistant":
			// Reasoning items come first, as the model produced them, and are replayed unchanged.
			for _, b := range msg.Blocks {
				if b.Type != "reasoning" {
					continue
				}
				items = append(items, InputItem{
					Type:             "reasoning",
					ID:               b.Signature,
					EncryptedContent: b.Data,
					Summary:          &[]string{},
				})
			}
			if phased := phasedMessages(msg); phased != nil {
				items = append(items, phased...)
			} else if msg.Content != "" {
				items = append(items, InputItem{
					Type:    "message",
					Role:    "assistant",
					Content: msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				items = append(items, InputItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		default:
			items = append(items, InputItem{
				Type:    "message",
				Role:    msg.Role,
				Content: msg.Content,
			})
		}
	}
	return items
}

// phasedMessages returns msg's message items with their phases, or nil when the model labelled
// none.
func phasedMessages(msg Message) []InputItem {
	var items []InputItem
	for _, b := range msg.Blocks {
		if b.Type == BlockResponsesMessage {
			items = append(items, InputItem{Type: "message", Role: "assistant", Content: b.Text, Phase: b.Signature})
		}
	}
	return items
}

// compactedWindowItems returns the items of a compacted window, the JSON array the compact
// endpoint returned, to be sent as they are. A window that does not decode, which only a damaged
// session file could hold, gives no items.
func compactedWindowItems(window string) []InputItem {
	var raw []jsontext.Value
	if err := json.Unmarshal([]byte(window), &raw); err != nil {
		return nil
	}
	items := make([]InputItem, len(raw))
	for i, v := range raw {
		items[i] = InputItem{Raw: v}
	}
	return items
}

func (c *Client) streamResponsesOnce(
	ctx context.Context,
	provider config.Provider,
	model config.Model,
	messages []Message,
	tools []ToolDefinition,
	onReasoning func(string),
	onDelta func(string),
) (Message, *TokenUsage, time.Duration, bool, error) {
	thinking := model.ThinkingEnabled()
	inputItems := convertMessagesToInputItems(messages)
	if !thinking {
		inputItems = dropReasoningItems(inputItems)
	}
	reqPayload := ResponsesRequest{
		Model:           model.ID,
		Input:           inputItems,
		Stream:          true,
		Tools:           responsesTools(tools),
		MaxOutputTokens: model.MaxOutputTokens,
	}
	if thinking {
		reqPayload.Include = []string{"reasoning.encrypted_content"}
		reqPayload.Reasoning = &responsesReasoning{Effort: model.Effort, Summary: "auto"}
	}

	body, err := encodeRequestBody(reqPayload)
	if err != nil {
		return Message{}, nil, 0, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL(provider.BaseURL), bytes.NewReader(body))
	if err != nil {
		return Message{}, nil, 0, false, err
	}

	req.Header.Set("Content-Type", "application/json")
	if key := config.ResolveAPIKey(provider.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	startTime := time.Now()
	acc := newResponsesAccumulator(startTime)
	acc.toolProgress = toolProgressFrom(ctx)
	retryAfter, retryable, err := c.doStream(ctx, req, func(evType, data string) error {
		return acc.handle(evType, data, onReasoning, onDelta)
	}, func() bool { return !acc.firstToken.IsZero() })
	if err != nil {
		return Message{}, nil, retryAfter, retryable, err
	}

	msg, usage := acc.result()
	return msg, usage, 0, false, nil
}

// responsesAccumulator folds the server-sent events of one Responses API stream into the
// assistant message and its token usage.
type responsesAccumulator struct {
	startTime time.Time
	// toolProgress is told about each tool call as the model writes it.
	toolProgress   toolProgress
	firstToken     time.Time
	contentBuilder strings.Builder
	toolCallsByID  map[string]*ToolCall
	toolArgsByID   map[string]*strings.Builder
	toolCallOrder  []string
	// callIDByItem maps output item IDs to call IDs, since argument events and
	// some output_item events identify the call only by item_id.
	callIDByItem map[string]string
	usage        *TokenUsage
	finishReason string
	// reasoning holds the reasoning items of the response, replayed in later turns.
	reasoning []Block
	// messages holds the response's message items that came with a phase.
	messages []Block
}

// reasoningBlock builds the replayable block for a completed reasoning output item. The item ID is
// kept in Signature, the encrypted content in Data and the summary text in Text.
func reasoningBlock(id, encrypted string, summary []struct {
	Text string `json:"text"`
}) Block {
	var text strings.Builder
	for _, s := range summary {
		text.WriteString(s.Text)
	}
	return Block{Type: "reasoning", Text: text.String(), Signature: id, Data: encrypted}
}

func newResponsesAccumulator(startTime time.Time) *responsesAccumulator {
	return &responsesAccumulator{
		startTime:     startTime,
		toolCallsByID: make(map[string]*ToolCall),
		toolArgsByID:  make(map[string]*strings.Builder),
		callIDByItem:  make(map[string]string),
	}
}

// handle applies one server-sent event. Text and reasoning deltas are passed to the
// callbacks as they arrive.
func (a *responsesAccumulator) handle(evType, data string, onReasoning, onDelta func(string)) error {
	if data == "[DONE]" {
		return nil
	}

	var ev responsesStreamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return err
	}

	eventType := evType
	if ev.Type != "" {
		eventType = ev.Type
	}

	switch eventType {
	case "response.output_text.delta", "response.text.delta":
		if ev.Delta != "" {
			a.markFirstToken()
			a.contentBuilder.WriteString(ev.Delta)
			if onDelta != nil {
				onDelta(ev.Delta)
			}
		}
	case "response.reasoning.delta", "response.output_reasoning.delta", "response.reasoning_summary_text.delta":
		if ev.Delta != "" {
			a.markFirstToken()
			if onReasoning != nil {
				onReasoning(ev.Delta)
			}
		}
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			a.markFirstToken()
			tc := a.toolCall(ev.Item.CallID, ev.Item.Name)
			if ev.Item.ID != "" {
				a.callIDByItem[ev.Item.ID] = tc.ID
			}
			a.toolProgress.report(tc.Function.Name, 0)
		}
	case "response.function_call_arguments.delta":
		a.markFirstToken()
		tc := a.toolCall(a.resolveCallID(ev.CallID, ev.ItemID), "")
		a.toolArgsByID[tc.ID].WriteString(ev.Delta)
		a.toolProgress.report(tc.Function.Name, a.toolArgsByID[tc.ID].Len())
	case "response.function_call_arguments.done":
		tc := a.toolCall(a.resolveCallID(ev.CallID, ev.ItemID), "")
		if ev.Arguments != "" {
			a.replaceArgs(tc.ID, ev.Arguments)
		}
	case "response.output_item.done":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			tc := a.toolCall(a.resolveCallID(ev.Item.CallID, ev.Item.ID), ev.Item.Name)
			if ev.Item.Arguments != "" {
				a.replaceArgs(tc.ID, ev.Item.Arguments)
			}
		}
		if ev.Item != nil && ev.Item.Type == "reasoning" {
			a.reasoning = append(a.reasoning, reasoningBlock(ev.Item.ID, ev.Item.EncryptedContent, ev.Item.Summary))
		}
		if ev.Item != nil && ev.Item.Type == "message" && ev.Item.Phase != "" {
			var text strings.Builder
			for _, part := range ev.Item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				}
			}
			a.messages = append(a.messages, Block{Type: BlockResponsesMessage, Text: text.String(), Signature: ev.Item.Phase})
		}
	case "error":
		return responsesStreamError(ev.Code, ev.Message, data)
	case "response.failed":
		if ev.Response != nil && ev.Response.Error != nil {
			return responsesStreamError(ev.Response.Error.Code, ev.Response.Error.Message, data)
		}
		return responsesStreamError("", "", data)
	case "response.completed", "response.done", "response.incomplete":
		if ev.Response != nil {
			if ev.Response.Usage != nil {
				a.usage = ev.Response.Usage
			}
			a.finishReason = responsesFinishReason(ev.Response.Status, ev.Response.IncompleteDetails)
		}
	}

	return nil
}

// responsesStreamError turns an "error" event, or the error of a "response.failed" one, into a Go
// error; data, the event, stands in when it gives no message. A server error or a rate limit is
// wrapped as retryableStreamError, so the retry loop can try again before output starts.
func responsesStreamError(code, message, data string) error {
	if message == "" {
		return fmt.Errorf("openai stream error: %s", truncateErrorBody([]byte(data), maxShownErrorRunes))
	}
	err := fmt.Errorf("openai error (%s): %s", cmp.Or(code, "no code"), message)
	switch code {
	case "server_error", "rate_limit_exceeded":
		return &retryableStreamError{err: err}
	}
	return err
}

// markFirstToken records the arrival time of the first model output, once.
func (a *responsesAccumulator) markFirstToken() {
	if a.firstToken.IsZero() {
		a.firstToken = time.Now()
	}
}

// resolveCallID returns callID when the event carries one. Otherwise it maps the
// event's output item ID to the call announced for that item, falling back to the
// most recent tool call.
func (a *responsesAccumulator) resolveCallID(callID, itemID string) string {
	if callID != "" {
		return callID
	}
	if id, ok := a.callIDByItem[itemID]; ok && itemID != "" {
		return id
	}
	if len(a.toolCallOrder) > 0 {
		return a.toolCallOrder[len(a.toolCallOrder)-1]
	}
	return ""
}

// responsesFinishReason maps a terminal Responses API status onto the finish
// reasons the agent understands: running out of output tokens becomes "length"
// (the agent flags truncation on it) and a completed response becomes "stop".
func responsesFinishReason(status string, details *responsesIncompleteDetails) string {
	switch {
	case status == "incomplete" && details != nil && details.Reason == "max_output_tokens":
		return "length"
	case status == "completed":
		return "stop"
	default:
		return status
	}
}

// toolCall returns the tool call for callID, creating it on first sight. A missing ID is
// replaced by a generated one, and a missing name is filled in once it arrives.
func (a *responsesAccumulator) toolCall(callID, name string) *ToolCall {
	if callID == "" {
		callID = fmt.Sprintf("call_%d", len(a.toolCallOrder)+1)
	}
	if tc, ok := a.toolCallsByID[callID]; ok {
		if name != "" && tc.Function.Name == "" {
			tc.Function.Name = name
		}
		return tc
	}
	tc := &ToolCall{
		ID:   callID,
		Type: "function",
		Function: ToolCallFunction{
			Name: name,
		},
	}
	a.toolCallsByID[callID] = tc
	a.toolArgsByID[callID] = &strings.Builder{}
	a.toolCallOrder = append(a.toolCallOrder, callID)
	return tc
}

// replaceArgs replaces the accumulated arguments of a tool call with the complete value.
func (a *responsesAccumulator) replaceArgs(callID, args string) {
	if b, ok := a.toolArgsByID[callID]; ok {
		b.Reset()
		b.WriteString(args)
	}
}

// result returns the assembled assistant message and the usage for the stream.
func (a *responsesAccumulator) result() (Message, *TokenUsage) {
	totalDuration := time.Since(a.startTime)
	var ttft time.Duration
	if !a.firstToken.IsZero() {
		ttft = a.firstToken.Sub(a.startTime)
	}

	usage := a.usage
	if usage != nil {
		if usage.PromptTokens == 0 && usage.InputTokens > 0 {
			usage.PromptTokens = usage.InputTokens
		}
		if usage.CompletionTokens == 0 && usage.OutputTokens > 0 {
			usage.CompletionTokens = usage.OutputTokens
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		if usage.PromptDetails == nil && usage.InputTokensDetails != nil {
			usage.PromptDetails = usage.InputTokensDetails
		}
		usage.Duration = totalDuration
		usage.TTFT = ttft
		if usage.CompletionTokens > 0 && !a.firstToken.IsZero() {
			genSec := time.Since(a.firstToken).Seconds()
			if genSec > 0 {
				usage.TokensPerSecond = float64(usage.CompletionTokens) / genSec
			}
		}
	} else {
		usage = &TokenUsage{
			Duration: totalDuration,
			TTFT:     ttft,
		}
	}
	usage.FinishReason = a.finishReason

	var toolCalls []ToolCall
	for _, callID := range a.toolCallOrder {
		if tc, ok := a.toolCallsByID[callID]; ok {
			if b, ok := a.toolArgsByID[callID]; ok && b.Len() > 0 {
				tc.Function.Arguments = b.String()
			}
			toolCalls = append(toolCalls, *tc)
		}
	}

	return Message{
		Role:      "assistant",
		Content:   a.contentBuilder.String(),
		ToolCalls: toolCalls,
		Blocks:    slices.Concat(a.reasoning, a.messages),
	}, usage
}
