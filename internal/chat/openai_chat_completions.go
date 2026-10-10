package chat

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"hmj-agent/internal/config"
)

// Legacy OpenAI Chat Completions Wire Types.
// All legacy completions types and logic are kept in this file to make
// deprecating and removing the legacy completions API trivial in the future.

type completionsRequest struct {
	Model         string                   `json:"model"`
	Messages      []completionsMessage     `json:"messages"`
	Stream        bool                     `json:"stream"`
	Tools         []completionsTool        `json:"tools,omitempty"`
	StreamOptions *completionsStreamOption `json:"stream_options,omitempty"`
	// MaxTokens caps the generated tokens; it is sent only when the model sets maxOutputTokens.
	MaxTokens int `json:"max_tokens,omitzero"`
}

type completionsStreamOption struct {
	IncludeUsage bool `json:"include_usage"`
}

type completionsMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type completionsTool struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type completionsChunk struct {
	Choices []completionsChoice `json:"choices"`
	Usage   *TokenUsage         `json:"usage"`
}

type completionsChoice struct {
	Index        int              `json:"index"`
	Delta        completionsDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type completionsDelta struct {
	Role             string                     `json:"role"`
	Content          string                     `json:"content"`
	ReasoningContent string                     `json:"reasoning_content"`
	Reasoning        string                     `json:"reasoning"`
	Thought          string                     `json:"thought"`
	ToolCalls        []completionsToolCallDelta `json:"tool_calls"`
}

type completionsToolCallDelta struct {
	Index    int                  `json:"index"`
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function completionsFuncDelta `json:"function"`
}

type completionsFuncDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func convertMessagesToCompletions(messages []Message) []completionsMessage {
	var msgs []completionsMessage
	for _, msg := range messages {
		switch msg.Role {
		case "tool":
			msgs = append(msgs, completionsMessage{
				Role:       "tool",
				Content:    msg.Content,
				ToolCallID: msg.ToolCallID,
			})
		case "assistant":
			m := completionsMessage{Role: "assistant", Content: msg.Content}
			m.ToolCalls = append(m.ToolCalls, msg.ToolCalls...)
			msgs = append(msgs, m)
		default:
			msgs = append(msgs, completionsMessage{Role: msg.Role, Content: msg.Content})
		}
	}
	return msgs
}

type completionsAccumulator struct {
	startTime        time.Time
	firstToken       time.Time
	contentBuilder   strings.Builder
	toolCallsByIndex map[int]*ToolCall
	toolArgsByIndex  map[int]*strings.Builder
	toolCallIndices  []int
	finishReason     string
	usage            *TokenUsage
}

func newCompletionsAccumulator(startTime time.Time) *completionsAccumulator {
	return &completionsAccumulator{
		startTime:        startTime,
		toolCallsByIndex: make(map[int]*ToolCall),
		toolArgsByIndex:  make(map[int]*strings.Builder),
	}
}

func (a *completionsAccumulator) handle(data string, onReasoning, onDelta func(string)) error {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" {
		return nil
	}

	var chunk completionsChunk
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
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
		return fmt.Errorf("chat completions stream: invalid chunk JSON: %w (snippet: %s)", err, snippet)
	}

	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}

	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			a.finishReason = *choice.FinishReason
		}

		reasoning := choice.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = choice.Delta.Reasoning
		}
		if reasoning == "" {
			reasoning = choice.Delta.Thought
		}
		if reasoning != "" {
			// Shown reasoning is delivered output: a retry would show it a second time.
			if a.firstToken.IsZero() {
				a.firstToken = time.Now()
			}
			if onReasoning != nil {
				onReasoning(reasoning)
			}
		}

		if choice.Delta.Content != "" {
			if a.firstToken.IsZero() {
				a.firstToken = time.Now()
			}
			a.contentBuilder.WriteString(choice.Delta.Content)
			if onDelta != nil {
				onDelta(choice.Delta.Content)
			}
		}

		for _, tcDelta := range choice.Delta.ToolCalls {
			idx := tcDelta.Index
			tc, exists := a.toolCallsByIndex[idx]
			if !exists {
				tc = &ToolCall{Type: "function"}
				a.toolCallsByIndex[idx] = tc
				a.toolArgsByIndex[idx] = &strings.Builder{}
				a.toolCallIndices = append(a.toolCallIndices, idx)
			}
			if tcDelta.ID != "" {
				tc.ID = tcDelta.ID
			}
			if tcDelta.Type != "" {
				tc.Type = tcDelta.Type
			}
			if tcDelta.Function.Name != "" {
				tc.Function.Name += tcDelta.Function.Name
			}
			if tcDelta.Function.Arguments != "" {
				if b, ok := a.toolArgsByIndex[idx]; ok {
					b.WriteString(tcDelta.Function.Arguments)
				}
			}
		}
	}

	return nil
}

func (a *completionsAccumulator) result() (Message, *TokenUsage) {
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
		usage.FinishReason = a.finishReason
		if usage.CompletionTokens > 0 && !a.firstToken.IsZero() {
			genSec := time.Since(a.firstToken).Seconds()
			if genSec > 0 {
				usage.TokensPerSecond = float64(usage.CompletionTokens) / genSec
			}
		}
	} else {
		usage = &TokenUsage{
			Duration:     totalDuration,
			TTFT:         ttft,
			FinishReason: a.finishReason,
		}
	}

	slices.Sort(a.toolCallIndices)
	toolCalls := make([]ToolCall, 0, len(a.toolCallIndices))
	for _, idx := range a.toolCallIndices {
		tc := a.toolCallsByIndex[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", idx+1)
		}
		if b, ok := a.toolArgsByIndex[idx]; ok && b.Len() > 0 {
			tc.Function.Arguments = b.String()
		}
		toolCalls = append(toolCalls, *tc)
	}

	return Message{
		Role:      "assistant",
		Content:   a.contentBuilder.String(),
		ToolCalls: toolCalls,
	}, usage
}

// streamCompletionsOnce executes one streaming request against the legacy
// OpenAI Chat Completions API (POST /chat/completions).
func (c *Client) streamCompletionsOnce(
	ctx context.Context,
	provider config.Provider,
	model config.Model,
	messages []Message,
	tools []ToolDefinition,
	onReasoning func(string),
	onDelta func(string),
) (Message, *TokenUsage, time.Duration, bool, error) {
	reqPayload := completionsRequest{
		Model:         model.ID,
		Messages:      convertMessagesToCompletions(messages),
		Stream:        true,
		StreamOptions: &completionsStreamOption{IncludeUsage: true},
		MaxTokens:     model.MaxOutputTokens,
	}
	if len(tools) > 0 {
		reqPayload.Tools = make([]completionsTool, 0, len(tools))
		for _, t := range tools {
			reqPayload.Tools = append(reqPayload.Tools, completionsTool{
				Type:     "function",
				Function: t.Function,
			})
		}
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return Message{}, nil, 0, false, err
	}

	baseURL := strings.TrimRight(provider.BaseURL, "/")
	var url string
	if strings.HasSuffix(baseURL, "/chat/completions") || strings.HasSuffix(baseURL, "/completions") {
		url = baseURL
	} else {
		url = baseURL + "/chat/completions"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Message{}, nil, 0, false, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key := config.ResolveAPIKey(provider.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	startTime := time.Now()
	acc := newCompletionsAccumulator(startTime)
	retryAfter, retryable, err := c.doStream(ctx, req, func(_, data string) error {
		return acc.handle(data, onReasoning, onDelta)
	}, func() bool { return !acc.firstToken.IsZero() })
	if err != nil {
		return Message{}, nil, retryAfter, retryable, err
	}

	msg, usage := acc.result()
	return msg, usage, 0, false, nil
}
