package chat

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"hmj-agent/internal/config"
	"hmj-agent/internal/logging"
)

// responsesCompactRequest is the body of a POST /responses/compact request.
type responsesCompactRequest struct {
	Model string      `json:"model"`
	Input []InputItem `json:"input"`
	// Instructions carries the system prompt, which stays out of the compacted window.
	Instructions string `json:"instructions,omitempty"`
}

// responsesCompactResponse is the part of the compact endpoint's reply that hmj reads.
type responsesCompactResponse struct {
	// Output is the compacted window: an encrypted compaction item, and possibly items kept
	// from the window sent. It is stored and sent back as it is.
	Output jsontext.Value `json:"output"`
	Usage  *TokenUsage    `json:"usage"`
}

// compactHeaderTimeout is how long the compact endpoint may take to start its reply. It writes the
// whole summary before it answers, which can take longer than the transport's header timeout for
// ordinary, streamed requests.
const compactHeaderTimeout = 10 * time.Minute

// maxCompactResponseBytes caps how much of the compact endpoint's reply is read.
const maxCompactResponseBytes = 64 << 20

// compactResponses compacts messages with the Responses API's compact endpoint
// (POST /responses/compact). The window sent is the conversation's input as the turns send it,
// without its system prompt, which goes as instructions and stays in place before the compacted
// window. The reply's output is the compacted window, kept as one BlockResponsesCompaction.
func (c *Client) compactResponses(ctx context.Context, provider config.Provider, model config.Model, messages []Message) (Message, error) {
	var system []string
	window := slices.DeleteFunc(slices.Clone(messages), func(m Message) bool {
		if m.Role == "system" {
			system = append(system, m.Content)
			return true
		}
		return false
	})
	input := convertMessagesToInputItems(window)
	if !model.ThinkingEnabled() {
		input = dropReasoningItems(input)
	}
	body, err := encodeRequestBody(responsesCompactRequest{Model: model.ID, Input: input, Instructions: strings.Join(system, "\n\n")})
	if err != nil {
		return Message{}, err
	}

	var reply responsesCompactResponse
	err = c.withRetries(ctx, func() (time.Duration, bool, error) {
		var retryAfter time.Duration
		var retryable bool
		var err error
		reply, retryAfter, retryable, err = c.sendResponsesCompact(ctx, provider, body)
		return retryAfter, retryable, err
	})
	if err != nil {
		return Message{}, err
	}

	var items []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(reply.Output, &items); err != nil {
		return Message{}, fmt.Errorf("compact endpoint: unreadable output: %w", err)
	}
	if !slices.ContainsFunc(items, func(it struct {
		Type string `json:"type"`
	}) bool {
		return it.Type == "compaction"
	}) {
		return Message{}, errors.New("no summary came back: the compacted window has no compaction item")
	}
	if reply.Usage != nil {
		logging.L().Info("compaction request", "prompt_tokens", reply.Usage.PromptTokens+reply.Usage.InputTokens,
			"cached_tokens", reply.Usage.EffectiveCachedTokens(), "output_tokens", reply.Usage.CompletionTokens+reply.Usage.OutputTokens, "items", len(items))
	}
	return Message{Role: "assistant", Blocks: []Block{{Type: BlockResponsesCompaction, Data: string(reply.Output)}}}, nil
}

// sendResponsesCompact sends one compact request and decodes its reply. Failures are classified
// for the retry loop as doStream classifies them.
func (c *Client) sendResponsesCompact(ctx context.Context, provider config.Provider, body []byte) (responsesCompactResponse, time.Duration, bool, error) {
	var reply responsesCompactResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL(provider.BaseURL)+"/compact", bytes.NewReader(body))
	if err != nil {
		return reply, 0, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := config.ResolveAPIKey(provider.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	sent := time.Now()
	resp, err := c.httpClientWithHeaderTimeout(compactHeaderTimeout).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return reply, 0, false, err
		}
		return reply, 0, IsRetryableNetError(err), err
	}
	defer resp.Body.Close()
	logResponse(req, resp, time.Since(sent))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpErrorReply(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCompactResponseBytes))
	if err != nil {
		if ctx.Err() != nil {
			return reply, 0, false, err
		}
		return reply, 0, IsRetryableNetError(err), err
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return reply, 0, false, fmt.Errorf("compact endpoint: invalid reply: %w", err)
	}
	return reply, 0, false, nil
}

// httpErrorReply reports a non-2xx compact reply as httpErrorFromResponse classifies it.
func httpErrorReply(resp *http.Response) (responsesCompactResponse, time.Duration, bool, error) {
	retryAfter, retryable, err := httpErrorFromResponse(resp)
	return responsesCompactResponse{}, retryAfter, retryable, err
}

// httpClientWithHeaderTimeout returns c's HTTP client with a response header timeout of d, or
// the client itself when its transport is not the standard one.
func (c *Client) httpClientWithHeaderTimeout(d time.Duration) *http.Client {
	t, ok := c.HTTPClient.Transport.(*http.Transport)
	if !ok {
		return c.HTTPClient
	}
	t = t.Clone()
	t.ResponseHeaderTimeout = d
	hc := *c.HTTPClient
	hc.Transport = t
	return &hc
}

// responsesURL returns the Responses endpoint for baseURL, accepting a base URL that already ends
// in /responses.
func responsesURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/responses") {
		return baseURL
	}
	return baseURL + "/responses"
}
