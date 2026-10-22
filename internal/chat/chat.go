package chat

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"hmj-agent/internal/config"
	"hmj-agent/internal/logging"
)

// Message is one turn of a chat transcript, either sent to or received from a model.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Blocks are provider-opaque content blocks of an assistant message, kept in
	// the order the model produced them. They must be replayed verbatim (for
	// example Anthropic thinking signatures), so they are not rendered or edited.
	Blocks []Block `json:"blocks,omitempty"`
}

// Block is one opaque provider content block that must be replayed unchanged in later requests.
// Anthropic uses Type "thinking" (Text, Signature) and "redacted_thinking" (Data). The OpenAI
// Responses API uses Type "reasoning" (Signature holds the item ID, Data the encrypted content).
type Block struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// ToolCall is a request from the model to run one of the advertised tools. ID links it to the tool
// result message that answers it.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
	// ThoughtSignature is Gemini's opaque signature for this call; it must be sent
	// back verbatim on the same function_call step in later turns.
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// ToolCallFunction names the function to run and carries its arguments as a JSON-encoded string.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition advertises a callable tool to the model.
type ToolDefinition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

// FunctionDefinition describes a callable function: its name, purpose and JSON Schema parameters.
type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// TokenUsage reports token counts and timing for one model response. The timing and finish-reason
// fields are measured by the client, not decoded from the provider.
type TokenUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	InputTokens         int                  `json:"input_tokens,omitempty"`
	OutputTokens        int                  `json:"output_tokens,omitempty"`
	CachedTokens        int                  `json:"cached_tokens,omitempty"`
	CacheReadTokens     int                  `json:"cache_read_input_tokens,omitempty"`
	InputTokensDetails  *PromptTokensDetails `json:"input_tokens_details,omitempty"`
	InputTokenDetails   *PromptTokensDetails `json:"input_token_details,omitempty"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	PromptDetails       *PromptTokensDetails `json:"prompt_details,omitempty"`

	Duration        time.Duration `json:"-"`
	TTFT            time.Duration `json:"-"`
	TokensPerSecond float64       `json:"-"`
	FinishReason    string        `json:"-"`
	// RefusalCategory is the safety category the provider gave for a refusal, when it reported one.
	RefusalCategory string `json:"-"`
	// Speed is the speed the provider reports the response ran at ("fast" or "standard" for
	// Anthropic), or empty when it reports none.
	Speed string `json:"-"`
}

// PromptTokensDetails holds prompt-cache counters, which providers report under different field
// names.
type PromptTokensDetails struct {
	CachedTokens         int `json:"cached_tokens,omitempty"`
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
	CachedPromptTokens   int `json:"cached_prompt_tokens,omitempty"`
}

// EffectiveCachedTokens returns the first non-zero cached-token count, or 0 when p is nil.
func (p *PromptTokensDetails) EffectiveCachedTokens() int {
	if p == nil {
		return 0
	}
	if p.CachedTokens > 0 {
		return p.CachedTokens
	}
	if p.CacheReadInputTokens > 0 {
		return p.CacheReadInputTokens
	}
	return p.CachedPromptTokens
}

// EffectiveCachedTokens returns the cached prompt-token count, checking the top-level fields before
// the nested details. It returns 0 when u is nil.
func (u *TokenUsage) EffectiveCachedTokens() int {
	if u == nil {
		return 0
	}
	if u.CachedTokens > 0 {
		return u.CachedTokens
	}
	if u.CacheReadTokens > 0 {
		return u.CacheReadTokens
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.EffectiveCachedTokens() > 0 {
		return u.InputTokensDetails.EffectiveCachedTokens()
	}
	if u.InputTokenDetails != nil && u.InputTokenDetails.EffectiveCachedTokens() > 0 {
		return u.InputTokenDetails.EffectiveCachedTokens()
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.EffectiveCachedTokens() > 0 {
		return u.PromptTokensDetails.EffectiveCachedTokens()
	}
	if u.PromptDetails != nil && u.PromptDetails.EffectiveCachedTokens() > 0 {
		return u.PromptDetails.EffectiveCachedTokens()
	}
	return 0
}

// Client sends chat requests to the configured providers and retries transient failures.
type Client struct {
	Config      config.Config
	RetryConfig RetryConfig
	OnRetry     RetryCallback
	HTTPClient  *http.Client
	// ConfigPath is the config file a detected protocol is written to as the provider's
	// "api" (see DetectWireAPI); empty disables that.
	ConfigPath string
	// OnProtocolDetected, when set, is called once a provider's protocol has been found by
	// probing, with whether it was saved to ConfigPath.
	OnProtocolDetected func(provider, api string, saved bool)
	// protocols remembers detection results for this process.
	protocols *protocolMemory
}

// NewClient returns a Client for cfg with the default retry policy and an HTTP client with no total
// request timeout, which suits long-running streams.
func NewClient(cfg config.Config) *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   30,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		// Without this, Go asks for gzip on every request. An event stream of small deltas
		// gains little from compression, and a compressing proxy may hold the stream back
		// before passing it on.
		DisableCompression: true,
	}

	return &Client{
		Config:      cfg,
		ConfigPath:  config.DefaultConfigPath(),
		RetryConfig: DefaultRetryConfig(),
		HTTPClient: &http.Client{
			Transport: transport,
			Timeout:   0, // Streaming requests have no fixed total timeout; context controls cancellation
			// Never follow redirects: Go drops Authorization on a cross-host redirect but
			// forwards x-api-key and x-goog-api-key, so a redirect could hand the key to
			// another host. The 3xx answer is reported instead (httpErrorFromResponse).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// WithModelOverride returns a copy of the client in which the model named by ref has the settings
// that change edits. The copy shares the protocol memory with c, and c itself is not changed. A ref
// that names no configured provider or model gives an unchanged copy.
func (c *Client) WithModelOverride(ref config.ModelRef, change func(*config.Model)) *Client {
	providers := c.Config.Providers
	p, ok := providers[ref.Provider]
	if !ok {
		return c.clone(c.Config)
	}
	models := slices.Clone(p.Models)
	for i := range models {
		if models[i].ID == ref.Model {
			change(&models[i])
		}
	}
	p.Models = models
	cfg := c.Config
	cfg.Providers = maps.Clone(providers)
	cfg.Providers[ref.Provider] = p
	return c.clone(cfg)
}

// clone returns a shallow copy of c that uses cfg, with its protocol memory created first so the
// copy and c share one memory.
func (c *Client) clone(cfg config.Config) *Client {
	c.protocolMemoryFor()
	cc := *c
	cc.Config = cfg
	return &cc
}

// StreamChat sends messages to the model named by ref and streams the reply, passing reasoning and
// answer text to onReasoning and onDelta as they arrive. Retryable failures are retried according
// to RetryConfig. It returns the final assistant message and its token usage.
func (c *Client) StreamChat(
	ctx context.Context,
	ref config.ModelRef,
	messages []Message,
	tools []ToolDefinition,
	onReasoning func(string),
	onDelta func(string),
) (Message, *TokenUsage, error) {
	provider, modelCfg, err := c.Config.ResolveModel(ref)
	if err != nil {
		return Message{}, nil, err
	}

	retryCfg := c.RetryConfig
	if retryCfg.MaxRetries < 0 {
		retryCfg = DefaultRetryConfig()
	}

	for attempt := 0; ; attempt++ {
		msg, usage, retryAfter, retryable, err := c.streamChatOnce(
			ctx,
			ref.Provider,
			provider,
			modelCfg,
			messages,
			tools,
			onReasoning,
			onDelta,
		)
		if err == nil {
			return msg, usage, nil
		}

		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Message{}, nil, err
		}

		if !retryable || attempt >= retryCfg.MaxRetries {
			return Message{}, nil, err
		}

		delay := min(time.Duration(float64(retryCfg.InitialBackoff)*math.Pow(retryCfg.BackoffFactor, float64(attempt))), retryCfg.MaxBackoff)
		// If the provider explicitly told us when the limit resets (a
		// Retry-After header or a "try again in 4m39s" body hint), honor that
		// exact window (plus a small buffer) instead of the naive backoff
		// schedule. Such hints can legitimately exceed MaxBackoff, e.g. an
		// hourly quota that resets in several minutes.
		if retryAfter > 0 {
			buf := retryCfg.RetryBuffer
			if buf == 0 {
				buf = retryAfterBuffer
			}
			delay = retryAfter + buf
		}
		// Jitter +/- 20% (skipped for server-provided exact waits, where we
		// want to land just after the reset, not before it).
		if retryAfter == 0 && delay > 0 {
			jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(delay))
			delay += jitter
		}
		if delay < 1*time.Millisecond {
			delay = 1 * time.Millisecond
		}

		logging.L().Warn("llm retry", "attempt", attempt+1, "max", retryCfg.MaxRetries, "delay", delay, "error", err.Error())
		if c.OnRetry != nil {
			c.OnRetry(attempt+1, retryCfg.MaxRetries, delay, err)
		}

		// The select below already wakes immediately if the user halts with
		// Esc (ctx cancellation), even for long quota-reset waits.
		select {
		case <-ctx.Done():
			return Message{}, nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *Client) streamChatOnce(
	ctx context.Context,
	providerName string,
	provider config.Provider,
	model config.Model,
	messages []Message,
	tools []ToolDefinition,
	onReasoning func(string),
	onDelta func(string),
) (Message, *TokenUsage, time.Duration, bool, error) {
	// Detection logs its own outcome once; a failed detection still runs the turn as Responses.
	api, _, _ := c.DetectWireAPI(ctx, providerName, provider)
	return c.streamChatWith(ctx, api, provider, model, messages, tools, onReasoning, onDelta)
}

// streamChatWith runs one streaming attempt with the builder for api.
func (c *Client) streamChatWith(
	ctx context.Context,
	api string,
	provider config.Provider,
	model config.Model,
	messages []Message,
	tools []ToolDefinition,
	onReasoning func(string),
	onDelta func(string),
) (Message, *TokenUsage, time.Duration, bool, error) {
	switch api {
	case config.APICompletions:
		return c.streamCompletionsOnce(ctx, provider, model, messages, tools, onReasoning, onDelta)
	case config.APIAnthropic:
		return c.streamAnthropicOnce(ctx, provider, model, messages, tools, onReasoning, onDelta)
	case config.APIGemini:
		return c.streamGeminiOnce(ctx, provider, model, messages, tools, onReasoning, onDelta)
	default:
		return c.streamResponsesOnce(ctx, provider, model, messages, tools, onReasoning, onDelta)
	}
}

// StreamIdleTimeout is how long a streaming response may go without delivering any bytes before
// the stream is treated as stalled and aborted. The HTTP client has no total timeout, so this is
// the only guard against a connection that silently stops making progress.
var StreamIdleTimeout = 120 * time.Second

// ErrStreamStalled reports that a streaming response delivered no bytes for StreamIdleTimeout.
var ErrStreamStalled = errors.New("stream stalled")

// retryableStreamError is an error the provider reports inside an otherwise successful stream,
// such as Anthropic's overloaded_error. It is retried when no output has been delivered yet.
type retryableStreamError struct {
	err error
}

func (e *retryableStreamError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying provider error.
func (e *retryableStreamError) Unwrap() error { return e.err }

// idleWatchdog cancels a stream when no bytes have been read for a full idle period. Every
// successful Read re-arms the timer.
type idleWatchdog struct {
	timer   *time.Timer
	idle    time.Duration
	stalled atomic.Bool
}

// newIdleWatchdog arms a watchdog that calls cancel after idle without a read.
func newIdleWatchdog(idle time.Duration, cancel context.CancelFunc) *idleWatchdog {
	w := &idleWatchdog{idle: idle}
	w.timer = time.AfterFunc(idle, func() {
		w.stalled.Store(true)
		cancel()
	})
	return w
}

// wrap returns a reader that re-arms the watchdog on every Read of r.
func (w *idleWatchdog) wrap(r io.ReadCloser) io.ReadCloser {
	return &idleReader{ReadCloser: r, w: w}
}

// Stalled reports whether the watchdog fired.
func (w *idleWatchdog) Stalled() bool { return w.stalled.Load() }

// Stop disarms the watchdog.
func (w *idleWatchdog) Stop() { w.timer.Stop() }

// idleReader resets its watchdog's timer on every Read.
type idleReader struct {
	io.ReadCloser
	w *idleWatchdog
}

// Read reads from the wrapped body and re-arms the idle timer.
func (r *idleReader) Read(p []byte) (int, error) {
	r.w.timer.Reset(r.w.idle)
	return r.ReadCloser.Read(p)
}

// logResponse records in the diagnostic log how a provider answered a model request: how long
// its response headers took, the HTTP version, any content encoding and the provider's request
// ID, which its support needs to look into a request.
func logResponse(req *http.Request, resp *http.Response, headersAfter time.Duration) {
	requestID := resp.Header.Get("request-id")
	if requestID == "" {
		requestID = resp.Header.Get("x-request-id")
	}
	logging.L().Debug("http response",
		"host", req.URL.Host,
		"status", resp.StatusCode,
		"proto", resp.Proto,
		"content_encoding", resp.Header.Get("Content-Encoding"),
		"request_id", requestID,
		"headers_after", headersAfter,
	)
}

// doStream sends req and feeds each server-sent event to onEvent. Failures are classified
// for the retry loop: context cancellation is never retryable, a network error or a stall
// before the first token (reported by started) is retryable, transient errors reported
// inside the stream (retryableStreamError) are retryable before the first token, and non-2xx
// responses are classified by httpErrorFromResponse.
func (c *Client) doStream(ctx context.Context, req *http.Request, onEvent func(eventType, data string) error, started func() bool) (time.Duration, bool, error) {
	// streamCtx is cancelled by the idle watchdog, which aborts the body read.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	sent := time.Now()
	resp, err := c.HTTPClient.Do(req.WithContext(streamCtx))
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, false, err
		}
		return 0, IsRetryableNetError(err), err
	}
	defer resp.Body.Close()
	logResponse(req, resp, time.Since(sent))

	watchdog := newIdleWatchdog(StreamIdleTimeout, cancelStream)
	defer watchdog.Stop()
	resp.Body = watchdog.wrap(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpErrorFromResponse(resp)
	}

	err = ReadSSE(resp.Body, onEvent)
	if err == nil {
		return 0, false, nil
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 0, false, err
	}
	if watchdog.Stalled() {
		return 0, !started(), fmt.Errorf("%w: no data for %s: %w", ErrStreamStalled, StreamIdleTimeout, err)
	}
	if _, ok := errors.AsType[*retryableStreamError](err); ok {
		return 0, !started(), err
	}
	return 0, !started() && IsRetryableNetError(err), err
}

// encodeRequestBody encodes a request body for a provider with map keys sorted. encoding/json/v2
// otherwise walks maps in random order, so a tool schema or replayed tool-call arguments changed key
// order from run to run, and the prompt the API caches changed with it.
func encodeRequestBody(v any) ([]byte, error) {
	return json.Marshal(v, json.Deterministic(true))
}
