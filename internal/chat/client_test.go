package chat_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/tools"
)

func TestStreamChatWithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		chunks := []string{
			`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_999","name":"read_file"}}`,
			`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_999","delta":"{\"path\":"}`,
			`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_999","delta":"\"main.go\"}"}`,
			`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_999","name":"read_file","arguments":"{\"path\":\"main.go\"}"}}`,
			`event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":120,"output_tokens":25,"total_tokens":145}}}`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: server.URL,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	msg, usage, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "mock", Model: "mock-model"},
		[]chat.Message{{Role: "user", Content: "read main.go"}},
		[]chat.ToolDefinition{(&tools.ReadFileTool{}).Definition()},
		func(thought string) {},
		func(delta string) {},
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if usage == nil || usage.TotalTokens != 145 {
		t.Fatalf("expected 145 total tokens, got %+v", usage)
	}

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}

	tc := msg.ToolCalls[0]
	if tc.ID != "call_999" {
		t.Fatalf("expected id call_999, got %s", tc.ID)
	}
	if tc.Function.Name != "read_file" {
		t.Fatalf("expected function name read_file, got %s", tc.Function.Name)
	}
	if !strings.Contains(tc.Function.Arguments, `"main.go"`) {
		t.Fatalf("expected arguments containing 'main.go', got %s", tc.Function.Arguments)
	}
}

func TestPromptCacheDetails(t *testing.T) {
	p1 := &chat.PromptTokensDetails{CachedTokens: 500}
	if p1.EffectiveCachedTokens() != 500 {
		t.Fatalf("expected 500, got %d", p1.EffectiveCachedTokens())
	}

	p2 := &chat.PromptTokensDetails{CacheReadInputTokens: 350}
	if p2.EffectiveCachedTokens() != 350 {
		t.Fatalf("expected 350, got %d", p2.EffectiveCachedTokens())
	}

	var pNil *chat.PromptTokensDetails
	if pNil.EffectiveCachedTokens() != 0 {
		t.Fatalf("expected 0 for nil, got %d", pNil.EffectiveCachedTokens())
	}
}

func TestStreamChatRetryOn429And500(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		switch count {
		case 1:
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		case 2:
			http.Error(w, `{"error":"server overloaded"}`, http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: response.output_text.delta\n")
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"recovered!"}`)
			fmt.Fprintf(w, "event: response.completed\n")
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}`)
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: server.URL,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	client.RetryConfig = chat.RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 2 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
		BackoffFactor:  1.5,
		RetryBuffer:    1 * time.Millisecond,
	}

	var retriesLogged atomic.Int32
	client.OnRetry = func(attempt, maxRetries int, delay time.Duration, err error) {
		retriesLogged.Add(1)
	}

	msg, _, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "mock", Model: "mock-model"},
		[]chat.Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)

	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if msg.Content != "recovered!" {
		t.Fatalf("expected 'recovered!', got: %q", msg.Content)
	}
	if requestCount.Load() != 3 {
		t.Fatalf("expected 3 requests (2 retries + 1 success), got: %d", requestCount.Load())
	}
	if retriesLogged.Load() != 2 {
		t.Fatalf("expected 2 OnRetry callbacks, got: %d", retriesLogged.Load())
	}
}

func TestStreamChatNonRetryableError(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized) // 401 should NOT be retried
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: server.URL,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	client.RetryConfig = chat.RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 5 * time.Millisecond,
	}

	var retriesLogged atomic.Int32
	client.OnRetry = func(attempt, maxRetries int, delay time.Duration, err error) {
		retriesLogged.Add(1)
	}

	_, _, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "mock", Model: "mock-model"},
		[]chat.Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)

	if err == nil {
		t.Fatal("expected error on 401 Unauthorized, got nil")
	}
	if requestCount.Load() != 1 {
		t.Fatalf("expected exactly 1 request (no retries for 401), got: %d", requestCount.Load())
	}
	if retriesLogged.Load() != 0 {
		t.Fatalf("expected 0 retries logged for 401, got: %d", retriesLogged.Load())
	}
}

func TestStreamChatMaxRetriesExhausted(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable) // 503
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: server.URL,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	client.RetryConfig = chat.RetryConfig{
		MaxRetries:     2,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
	}

	var retriesLogged atomic.Int32
	client.OnRetry = func(attempt, maxRetries int, delay time.Duration, err error) {
		retriesLogged.Add(1)
	}

	_, _, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "mock", Model: "mock-model"},
		[]chat.Message{{Role: "user", Content: "hi"}},
		nil,
		nil,
		nil,
	)

	if err == nil {
		t.Fatal("expected error when retries exhausted, got nil")
	}
	if requestCount.Load() != 3 { // 1 initial + 2 retries = 3
		t.Fatalf("expected 3 requests, got: %d", requestCount.Load())
	}
	if retriesLogged.Load() != 2 {
		t.Fatalf("expected 2 retries logged, got: %d", retriesLogged.Load())
	}
}

func TestStreamChatOpenAIResponsesSpec(t *testing.T) {
	var capturedPath string
	var capturedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		bodyBytes, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("failed to read request body: %v", readErr)
		}
		capturedBody = string(bodyBytes)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			"event: response.reasoning.delta\ndata: {\"type\":\"response.reasoning.delta\",\"delta\":\"analyzing file\"}",
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"item_1\",\"type\":\"function_call\",\"call_id\":\"call_resp_1\",\"name\":\"read_file\"}}",
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"call_id\":\"call_resp_1\",\"delta\":\"{\\\"path\\\":\\\"main.go\\\"}\"}",
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"item_1\",\"type\":\"function_call\",\"call_id\":\"call_resp_1\",\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"main.go\\\"}\"}}",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Here is the file summary.\"}",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":210,\"output_tokens\":40,\"total_tokens\":250,\"input_tokens_details\":{\"cached_tokens\":50}}}}",
			"data: [DONE]",
		}

		for _, ev := range events {
			fmt.Fprintf(w, "%s\n\n", ev)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"opencode": {
				BaseURL: server.URL + "/v1",
				API:     "responses",
				Models: []config.Model{
					{
						ID: "muse-spark",
					},
				},
			},
		},
	}

	client := chat.NewClient(cfg)
	var reasoningTokens []string
	var deltaTokens []string

	msg, usage, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "opencode", Model: "muse-spark"},
		[]chat.Message{{Role: "user", Content: "check main.go"}},
		[]chat.ToolDefinition{(&tools.ReadFileTool{}).Definition()},
		func(r string) { reasoningTokens = append(reasoningTokens, r) },
		func(d string) { deltaTokens = append(deltaTokens, d) },
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedPath != "/v1/responses" {
		t.Fatalf("expected request path /v1/responses, got: %s", capturedPath)
	}

	if !strings.Contains(capturedBody, `"type":"message"`) || !strings.Contains(capturedBody, `"role":"user"`) {
		t.Fatalf("expected input items array with message and user role, got: %s", capturedBody)
	}

	if strings.Join(reasoningTokens, "") != "analyzing file" {
		t.Fatalf("expected reasoning 'analyzing file', got: %q", strings.Join(reasoningTokens, ""))
	}

	if strings.Join(deltaTokens, "") != "Here is the file summary." {
		t.Fatalf("expected delta 'Here is the file summary.', got: %q", strings.Join(deltaTokens, ""))
	}

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_resp_1" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"main.go"}` {
		t.Fatalf("unexpected tool call: %+v", tc)
	}

	if usage == nil || usage.TotalTokens != 250 || usage.PromptTokens != 210 || usage.CompletionTokens != 40 {
		t.Fatalf("unexpected usage normalization: %+v", usage)
	}
	if usage.EffectiveCachedTokens() != 50 {
		t.Fatalf("expected 50 cached tokens, got %d", usage.EffectiveCachedTokens())
	}
}

func TestParseRateLimitHint(t *testing.T) {
	cases := []struct {
		body string
		want time.Duration
	}{
		{"You've hit your hourly rate limit. Please try again in 4m39s. Add a subscription: https://hyper.charm.land", 4*time.Minute + 39*time.Second},
		{"You've hit your hourly rate limit. Please try again in 28m18s. Add a subscription to increase your rate limits: https://hyper.charm.land", 28*time.Minute + 18*time.Second},
		{"Too many requests, retry in 90 seconds", 90 * time.Second},
		{"try again in 5 minutes", 5 * time.Minute},
		{"try again in 2 hours and 15 mins", 2*time.Hour + 15*time.Minute},
		{"retry in 90 secs", 90 * time.Second},
		{"try again in 500 milliseconds", 500 * time.Millisecond},
		{"rate limit exceeded, try again in 1h2m3s", time.Hour + 2*time.Minute + 3*time.Second},
		{"Please try again in 30s", 30 * time.Second},
		{"Please try again in 1.5m", 90 * time.Second},
		{"", 0},
		{"unrelated error: something failed", 0},
		{"we have no interest in your request", 0},
		{"server error, please retry later", 0},
		{"try again in the morning", 0},
		// "months" must not be misread as minutes.
		{"retry in 15 months", 0},
		// "hours" alone (no digits) yields no duration.
		{"try again in hours", 0},
	}
	for _, c := range cases {
		got := chat.ParseRateLimitHint(c.body)
		if got != c.want {
			t.Errorf("ParseRateLimitHint(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestStreamChatHonorsBodyHintOn429 verifies that a 429 whose body carries a
// human-readable reset hint (no Retry-After header) triggers a retry that waits
// the exact hinted window instead of the naive backoff schedule.
func TestStreamChatHonorsBodyHintOn429(t *testing.T) {
	var requestCount atomic.Int32
	var firstRequestAt time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		switch count {
		case 1:
			firstRequestAt = time.Now()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":{"message":"You've hit your hourly rate limit. Please try again in 50ms.","type":"rate_limit_error","code":null}}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: response.output_text.delta\n")
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.output_text.delta","delta":"recovered!"}`)
			fmt.Fprintf(w, "event: response.completed\n")
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}`)
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: server.URL,
				API:     "responses",
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	client.RetryConfig = chat.RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond, // naive backoff (20ms max) would never reach the 50ms hint
		BackoffFactor:  2.0,
		RetryBuffer:    5 * time.Millisecond,
	}

	start := time.Now()
	_, _, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "mock", Model: "mock-model"},
		[]chat.Message{{Role: "user", Content: "hello"}},
		nil,
		func(string) {}, func(string) {},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if requestCount.Load() != 2 {
		t.Fatalf("expected 2 requests, got %d", requestCount.Load())
	}
	// With the hint honored, the wait is ~50ms+buffer; naive backoff+capped
	// jitter would have completed in well under 25ms.
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("expected >= 40ms wait honoring the body hint, elapsed: %v", elapsed)
	}
	if firstRequestAt.IsZero() {
		t.Fatal("expected first request timestamp to be recorded by the mock server")
	}
}

func TestParseRetryAfterMatrix(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"not-a-number", 0},
		{"0", 0},
		{"-5", 0},
		{"2", 2 * time.Second},
		{"3600", time.Hour},
	}
	for _, tc := range cases {
		got := chat.ParseRetryAfterHeader(tc.in)
		if tc.want == 0 && got != 0 {
			t.Errorf("ParseRetryAfterHeader(%q) = %v, want 0", tc.in, got)
		}
		if tc.want > 0 && got != tc.want {
			t.Errorf("ParseRetryAfterHeader(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseRateLimitHintMatrix(t *testing.T) {
	cases := []struct {
		body string
		want time.Duration
	}{
		{`{"error":{"message":"try again in 4m39s"}}`, 4*time.Minute + 39*time.Second},
		{`{"error":{"message":"retry in 90 secs"}}`, 90 * time.Second},
		{`{"error":{"message":"try again in 2 hours and 15 mins"}}`, 2*time.Hour + 15*time.Minute},
		{`{"error":{"message":"no hint here"}}`, 0},
		{``, 0},
		{`garbage`, 0},
	}
	for _, tc := range cases {
		got := chat.ParseRateLimitHint(tc.body)
		if got != tc.want {
			t.Errorf("ParseRateLimitHint(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestIsRetryableStatusCodeMatrix(t *testing.T) {
	retryable := []int{429, 408, 500, 502, 503, 504}
	for _, code := range retryable {
		if !chat.IsRetryableStatusCode(code) {
			t.Errorf("expected %d to be retryable", code)
		}
	}
	fatal := []int{400, 401, 403, 404, 422}
	for _, code := range fatal {
		if chat.IsRetryableStatusCode(code) {
			t.Errorf("expected %d to be non-retryable", code)
		}
	}
}

func TestIsRetryableNetErrorMatrix(t *testing.T) {
	if !chat.IsRetryableNetError(errors.New("connection reset by peer")) {
		t.Error("expected connection reset to be retryable")
	}
	if !chat.IsRetryableNetError(errors.New("read: connection refused")) {
		t.Error("expected connection refused to be retryable")
	}
	if !chat.IsRetryableNetError(&net.OpError{Op: "dial", Err: errors.New("timeout")}) {
		t.Error("expected wrapped timeout to be retryable")
	}
	if chat.IsRetryableNetError(errors.New("some totally unexpected failure")) {
		t.Error("expected unknown error to be non-retryable")
	}
	if chat.IsRetryableNetError(nil) {
		t.Error("expected nil to be non-retryable")
	}
}

func TestEffectiveCachedTokensBranches(t *testing.T) {
	u := &chat.TokenUsage{}
	if got := u.EffectiveCachedTokens(); got != 0 {
		t.Errorf("empty usage: want 0, got %d", got)
	}
	u.CachedTokens = 42
	if got := u.EffectiveCachedTokens(); got != 42 {
		t.Errorf("legacy CachedTokens: want 42, got %d", got)
	}
}

func TestReadSSEHandlesMalformedLines(t *testing.T) {
	stream := "data: not-json\n\n" +
		"data: {\"broken\": \n\n" +
		": comment only\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"\n\n" // empty event
	var got []string
	err := chat.ReadSSE(strings.NewReader(stream), func(eventType, data string) error {
		got = append(got, eventType+"|"+data)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE returned error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one valid event to be delivered")
	}
}

func TestStreamChatNonRetryableFailsFast(t *testing.T) {
	server := newOneShotSSEServer(t, http.StatusBadRequest, `{"error":{"message":"bad request"}}`)
	cfg := chatConfigFor(server.URL)
	client := chat.NewClient(cfg)
	client.RetryConfig = chat.RetryConfig{MaxRetries: 3, InitialBackoff: time.Millisecond}

	start := time.Now()
	_, _, err := client.StreamChat(t.Context(), chatModelRef(), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("expected fail-fast, took %v", elapsed)
	}
}

func newOneShotSSEServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("failed to write response body: %v", err)
		}
	}))
}

func chatConfigFor(baseURL string) config.Config {
	return config.Config{
		Providers: map[string]config.Provider{
			"mock": {
				BaseURL: baseURL,
				Models:  []config.Model{{ID: "mock-model"}},
			},
		},
	}
}

func chatModelRef() config.ModelRef {
	return config.ModelRef{Provider: "mock", Model: "mock-model"}
}

func TestStreamChatAnthropicBadEventsSurfaceError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Send message_start with invalid JSON for the message payload
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":\"not-an-object\"}\n\n")
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"p": {
				BaseURL: server.URL,
				APIKey:  "sk-test",
				API:     "anthropic",
				Models:  []config.Model{{ID: "m"}},
			},
		},
	}
	client := chat.NewClient(cfg)
	_, _, err := client.StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error on malformed message_start event, got nil")
	}
	if !strings.Contains(err.Error(), "invalid event JSON") {
		t.Fatalf("expected unmarshal error message, got: %v", err)
	}
}

func TestStreamChatAnthropicErrorEvent(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		wantErr string
	}{
		{
			"provider error surfaces type and message",
			`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be positive"}}`,
			"anthropic error (invalid_request_error): max_tokens: must be positive",
		},
		{
			"malformed error payload keeps the raw event",
			`{"type":"error","error":"boom"}`,
			`invalid event JSON`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", tc.event)
			}))
			defer server.Close()

			cfg := config.Config{Providers: map[string]config.Provider{
				"p": {BaseURL: server.URL, APIKey: "k", API: "anthropic", Models: []config.Model{{ID: "m"}}},
			}}
			var streamed strings.Builder
			_, _, err := chat.NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"},
				nil, nil, nil, func(d string) { streamed.WriteString(d) })
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if streamed.String() != "partial" {
				t.Errorf("text before the error = %q, want partial", streamed.String())
			}
		})
	}
}

func TestResponsesAccumulatorIncrementalChunks(t *testing.T) {
	sseData := `event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_123","name":"edit_file"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_123","delta":"{\"path\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_123","delta":"\"main.go\""}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","call_id":"call_123","delta":",\"content\":\"test\"}"}

event: response.completed
data: {"type":"response.completed","response":{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}}

data: [DONE]
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseData))
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"test": {
				BaseURL: server.URL + "/responses",
				API:     "responses",
				Models:  []config.Model{{ID: "test-model"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	ref := config.ModelRef{Provider: "test", Model: "test-model"}
	msg, usage, err := client.StreamChat(context.Background(), ref, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}

	expectedArgs := `{"path":"main.go","content":"test"}`
	if msg.ToolCalls[0].Function.Arguments != expectedArgs {
		t.Fatalf("expected tool arguments %q, got %q", expectedArgs, msg.ToolCalls[0].Function.Arguments)
	}

	if usage == nil || usage.TotalTokens != 30 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestStreamChatLegacyCompletions(t *testing.T) {
	var receivedPath string
	var receivedBody []byte
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		var readErr error
		receivedBody, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("failed to read request body: %v", readErr)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Thinking..."},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":" legacy world!"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_legacy_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"test.txt\"}"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"legacy-provider": {
				BaseURL: server.URL,
				APIKey:  "test-key-123",
				API:     "completions",
				Models:  []config.Model{{ID: "legacy-gpt"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	var reasoningBuilder strings.Builder
	var deltaBuilder strings.Builder

	msg, usage, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "legacy-provider", Model: "legacy-gpt"},
		[]chat.Message{{Role: "user", Content: "hello"}},
		[]chat.ToolDefinition{(&tools.ReadFileTool{}).Definition()},
		func(thought string) { reasoningBuilder.WriteString(thought) },
		func(delta string) { deltaBuilder.WriteString(delta) },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedPath != "/chat/completions" {
		t.Errorf("expected path /chat/completions, got %s", receivedPath)
	}
	if receivedAuth != "Bearer test-key-123" {
		t.Errorf("expected auth Bearer test-key-123, got %s", receivedAuth)
	}

	// Verify request body had legacy structure
	var parsedBody map[string]any
	if err := json.Unmarshal(receivedBody, &parsedBody); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	if parsedBody["model"] != "legacy-gpt" {
		t.Errorf("expected model legacy-gpt, got %v", parsedBody["model"])
	}
	if parsedBody["stream"] != true {
		t.Errorf("expected stream=true, got %v", parsedBody["stream"])
	}

	// Verify streaming output
	if reasoningBuilder.String() != "Thinking..." {
		t.Errorf("expected reasoning 'Thinking...', got %q", reasoningBuilder.String())
	}
	if deltaBuilder.String() != "Hello legacy world!" {
		t.Errorf("expected delta 'Hello legacy world!', got %q", deltaBuilder.String())
	}
	if msg.Content != "Hello legacy world!" {
		t.Errorf("expected msg content 'Hello legacy world!', got %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "call_legacy_1" || msg.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("unexpected tool call: %+v", msg.ToolCalls[0])
	}
	if usage == nil || usage.TotalTokens != 30 || usage.FinishReason != "tool_calls" {
		t.Errorf("unexpected usage: %+v", usage)
	}
}

func TestStreamChatAnthropicMessages(t *testing.T) {
	var receivedPath string
	var receivedKey string
	var receivedVer string
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedKey = r.Header.Get("x-api-key")
		receivedVer = r.Header.Get("anthropic-version")
		var readErr error
		receivedBody, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("failed to read request body: %v", readErr)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_anthropic_1\",\"model\":\"claude-sonnet-5-5\",\"usage\":{\"input_tokens\":50,\"output_tokens\":1,\"cache_read_input_tokens\":10}}}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Analyzing code...\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Here is \"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"the result:\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_ant_1\",\"name\":\"read_file\",\"input\":{}}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"main\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\".go\\\"}\"}}",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":35}}",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}",
		}

		for _, ev := range events {
			fmt.Fprintf(w, "%s\n\n", ev)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Providers: map[string]config.Provider{
			"anthropic-provider": {
				BaseURL: server.URL,
				APIKey:  "sk-ant-testkey",
				API:     "anthropic",
				Models:  []config.Model{{ID: "claude-sonnet-5-5"}},
			},
		},
	}

	client := chat.NewClient(cfg)
	var reasoningBuilder strings.Builder
	var deltaBuilder strings.Builder

	msg, usage, err := client.StreamChat(
		context.Background(),
		config.ModelRef{Provider: "anthropic-provider", Model: "claude-sonnet-5-5"},
		[]chat.Message{
			{Role: "system", Content: "You are a coding assistant."},
			{Role: "user", Content: "Show me main.go"},
		},
		[]chat.ToolDefinition{(&tools.ReadFileTool{}).Definition()},
		func(thought string) { reasoningBuilder.WriteString(thought) },
		func(delta string) { deltaBuilder.WriteString(delta) },
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedPath != "/v1/messages" {
		t.Errorf("expected path /v1/messages, got %s", receivedPath)
	}
	if receivedKey != "sk-ant-testkey" {
		t.Errorf("expected x-api-key sk-ant-testkey, got %s", receivedKey)
	}
	if receivedVer != "2026-10-21" {
		t.Errorf("expected anthropic-version 2026-10-21, got %s", receivedVer)
	}

	// Verify request payload hoisted system prompt (as a content block with a
	// prompt-caching breakpoint) and formatted tools
	var reqObj struct {
		Model  string `json:"model"`
		System []struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
		MaxTokens int `json:"max_tokens"`
		Tools     []struct {
			Name         string         `json:"name"`
			InputSchema  map[string]any `json:"input_schema"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(receivedBody, &reqObj); err != nil {
		t.Fatalf("failed to unmarshal anthropic request body: %v", err)
	}
	if len(reqObj.System) != 1 || reqObj.System[0].Text != "You are a coding assistant." {
		t.Errorf("expected hoisted system prompt block, got %+v", reqObj.System)
	}
	if len(reqObj.System) == 1 {
		if reqObj.System[0].CacheControl == nil || reqObj.System[0].CacheControl.Type != "ephemeral" {
			t.Errorf("expected ephemeral cache_control breakpoint on system prompt, got %+v", reqObj.System[0].CacheControl)
		}
	}
	if reqObj.MaxTokens != chat.DefaultAnthropicMaxTokens {
		t.Errorf("expected max_tokens=%d, got %d", chat.DefaultAnthropicMaxTokens, reqObj.MaxTokens)
	}
	if len(reqObj.Tools) != 1 || reqObj.Tools[0].Name != "read_file" {
		t.Errorf("unexpected anthropic tools: %+v", reqObj.Tools)
	}

	// Verify accumulated output
	if reasoningBuilder.String() != "Analyzing code..." {
		t.Errorf("expected reasoning 'Analyzing code...', got %q", reasoningBuilder.String())
	}
	if deltaBuilder.String() != "Here is the result:" {
		t.Errorf("expected text 'Here is the result:', got %q", deltaBuilder.String())
	}
	if msg.Content != "Here is the result:" {
		t.Errorf("expected msg content 'Here is the result:', got %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "toolu_ant_1" || msg.ToolCalls[0].Function.Name != "read_file" || msg.ToolCalls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("unexpected tool call: %+v", msg.ToolCalls[0])
	}
	// PromptTokens is the full prompt: 50 uncached input tokens plus 10 cache reads.
	if usage == nil || usage.PromptTokens != 60 || usage.CompletionTokens != 35 || usage.TotalTokens != 95 {
		t.Errorf("unexpected usage: %+v", usage)
	}
	if usage.EffectiveCachedTokens() != 10 {
		t.Errorf("expected 10 cached tokens, got %d", usage.EffectiveCachedTokens())
	}
	if usage.FinishReason != "tool_calls" {
		t.Errorf("expected finish reason 'tool_calls', got %q", usage.FinishReason)
	}
}

// TestIsRetryableNetError covers transient network error classification.
func TestIsRetryableNetError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{io.EOF, true},
		{io.ErrUnexpectedEOF, true},
		{errors.New("connection reset by peer"), true},
		{errors.New("broken pipe"), true},
		{errors.New("connection refused"), true},
		{context.Canceled, false},
		{errors.New("some random failure"), false},
	}
	for _, tc := range cases {
		if got := chat.IsRetryableNetError(tc.err); got != tc.want {
			t.Errorf("IsRetryableNetError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A provider error is shown as the provider wrote it, and a long body is cut to a readable length.
func TestStreamChatErrorBodyIsShort(t *testing.T) {
	const key = "sk-proj-0123456789abcdefghijklmnop"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Incorrect API key provided: `+key+`"}}`+strings.Repeat(" padding", 1000), http.StatusUnauthorized)
	}))
	defer server.Close()
	cfg := config.Config{Providers: map[string]config.Provider{"mock": {BaseURL: server.URL, APIKey: key, API: "responses", Models: []config.Model{{ID: "m"}}}}}

	_, _, err := chat.NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "mock", Model: "m"},
		[]chat.Message{{Role: "user", Content: "hi"}}, nil, nil, nil)
	var statusErr *chat.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want a StatusError", err)
	}
	if !strings.Contains(err.Error(), "Incorrect API key provided: "+key) {
		t.Errorf("error does not show the provider's message: %v", err)
	}
	if n := len([]rune(statusErr.Body)); n > 1100 {
		t.Errorf("shown body is %d runes, want about 1000", n)
	}
}
