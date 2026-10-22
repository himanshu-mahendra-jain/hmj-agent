package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hmj-agent/internal/config"
)

// clientFor returns a client with one provider "p" speaking api at baseURL, model "m", and
// retries that wait only a millisecond.
func clientFor(api, baseURL string, retries int) *Client {
	c := NewClient(config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: baseURL, APIKey: "k", API: api, Models: []config.Model{{ID: "m"}}},
	}})
	c.RetryConfig = RetryConfig{MaxRetries: retries, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, BackoffFactor: 1}
	return c
}

// allAPIs lists every wire protocol a provider can speak.
var allAPIs = []string{config.APIResponses, config.APICompletions, config.APIAnthropic, config.APIGemini}

// TestStreamChatUnknownModel checks that a reference to a model the config does not list fails
// before any request is sent.
func TestStreamChatUnknownModel(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer server.Close()

	_, _, err := clientFor(config.APIResponses, server.URL, 0).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "nope"}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error for a model the config does not list")
	}
	if hits.Load() != 0 {
		t.Errorf("server got %d requests, want none", hits.Load())
	}
}

// TestStreamChatNegativeMaxRetriesUsesDefaults checks that a negative MaxRetries falls back to the
// default policy rather than disabling the request: a non-retryable error is still returned after
// exactly one attempt.
func TestStreamChatNegativeMaxRetriesUsesDefaults(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":{"message":"bad"}}`, http.StatusBadRequest)
	}))
	defer server.Close()

	c := clientFor(config.APIResponses, server.URL, 0)
	c.RetryConfig.MaxRetries = -1
	_, _, err := c.StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1 (400 is not retryable)", hits.Load())
	}
}

// TestRetryAfterAddsDefaultBufferAndHaltsDuringWait checks that a server-provided Retry-After wait
// gets the default buffer when RetryBuffer is zero, and that cancelling during the wait returns at
// once with the cancellation instead of sleeping it out.
func TestRetryAfterAddsDefaultBufferAndHaltsDuringWait(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "30")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := clientFor(config.APIResponses, server.URL, 3)
	c.RetryConfig.RetryBuffer = 0
	var gotDelay time.Duration
	c.OnRetry = func(attempt, max int, delay time.Duration, err error) {
		gotDelay = delay
		cancel() // the user halts while hmj waits for the quota to reset
	}

	start := time.Now()
	_, _, err := c.StreamChat(ctx, testRef, nil, nil, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if want := 30*time.Second + retryAfterBuffer; gotDelay != want {
		t.Errorf("retry delay = %s, want Retry-After plus the default buffer (%s)", gotDelay, want)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("halting during the wait took %s", elapsed)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}

// TestStreamChatAlreadyCancelled checks that a turn whose context is already cancelled returns the
// cancellation without retrying.
func TestStreamChatAlreadyCancelled(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := clientFor(config.APIResponses, server.URL, 3)
	var retries int
	c.OnRetry = func(int, int, time.Duration, error) { retries++ }
	if _, _, err := c.StreamChat(ctx, testRef, nil, nil, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0", retries)
	}
}

// TestConnectionRefusedIsRetried checks that a network failure before any response is retried, and
// the last error is returned once the retries run out.
func TestConnectionRefusedIsRetried(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // nothing listens there any more

	c := clientFor(config.APIResponses, url, 2)
	var retries int
	c.OnRetry = func(int, int, time.Duration, error) { retries++ }
	_, _, err := c.StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	if err == nil || !IsRetryableNetError(err) {
		t.Fatalf("err = %v, want a retryable network error", err)
	}
	if retries != 2 {
		t.Errorf("retries = %d, want 2", retries)
	}
}

// TestRetryableStreamErrorUnwraps checks that an in-stream provider error that is retried keeps
// the provider's own error reachable through errors.Unwrap.
func TestRetryableStreamErrorUnwraps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
	}))
	defer server.Close()

	_, _, err := anthropicTestClient(server.URL, 0).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	rse, ok := errors.AsType[*retryableStreamError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want a retryableStreamError", err, err)
	}
	inner := errors.Unwrap(rse)
	if inner == nil || inner.Error() != rse.Error() || !strings.Contains(inner.Error(), "overloaded_error") {
		t.Errorf("Unwrap = %v, want the provider's overloaded_error", inner)
	}
}

// TestUnreadableErrorBodyIsReported checks that an error response whose body breaks off mid-read
// still yields a StatusError with the status, a note about the failed read, and the read error
// reachable through errors.Is.
func TestUnreadableErrorBodyIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "short") // the connection closes before the promised 1000 bytes
	}))
	defer server.Close()

	_, _, err := clientFor(config.APIResponses, server.URL, 0).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
	if !strings.Contains(se.Body, "failed to read error response") {
		t.Errorf("Body = %q, want a note about the failed read", se.Body)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
}

// TestUnencodableToolSchemaIsError checks, for every protocol, that a tool whose schema cannot be
// encoded as JSON fails the call without sending anything.
func TestUnencodableToolSchemaIsError(t *testing.T) {
	tools := []ToolDefinition{{Type: "function", Function: FunctionDefinition{
		Name:       "bad",
		Parameters: map[string]any{"type": "object", "default": make(chan int)},
	}}}
	for _, api := range allAPIs {
		t.Run(api, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
			defer server.Close()

			msgs := []Message{{Role: "user", Content: "hi"}}
			_, _, err := clientFor(api, server.URL, 2).StreamChat(context.Background(), testRef, msgs, tools, nil, nil)
			if err == nil {
				t.Fatal("expected an encoding error")
			}
			if hits.Load() != 0 {
				t.Errorf("server got %d requests, want none", hits.Load())
			}
		})
	}
}

// TestInvalidBaseURLIsError checks, for every protocol, that a base URL that cannot form a request
// fails at once and is not retried.
func TestInvalidBaseURLIsError(t *testing.T) {
	for _, api := range allAPIs {
		t.Run(api, func(t *testing.T) {
			c := clientFor(api, "http://bad host", 2)
			var retries int
			c.OnRetry = func(int, int, time.Duration, error) { retries++ }
			_, _, err := c.StreamChat(context.Background(), testRef, nil, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "invalid character") {
				t.Fatalf("err = %v, want a URL parse error", err)
			}
			if retries != 0 {
				t.Errorf("retries = %d, want 0", retries)
			}
		})
	}
}

// TestEndpointURLSuffixes checks that a base URL that already names the protocol's endpoint is
// used as it is instead of getting the path appended a second time.
func TestEndpointURLSuffixes(t *testing.T) {
	tests := []struct {
		api, path, body string
	}{
		{config.APIResponses, "/v1/responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"},
		{config.APICompletions, "/v1/completions", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"},
		{config.APICompletions, "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"},
		{config.APIAnthropic, "/v1/messages", "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" + anthropicEndTurn},
		{config.APIAnthropic, "/proxy/messages", "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" + anthropicEndTurn},
	}
	for _, tc := range tests {
		t.Run(tc.api+tc.path, func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
			}))
			defer server.Close()

			msg, _, err := clientFor(tc.api, server.URL+tc.path+"/", 0).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != tc.path || msg.Content != "ok" {
				t.Errorf("request path %q, content %q; want %q and ok", gotPath, msg.Content, tc.path)
			}
		})
	}
}

// TestReadSSEJoinsMultiLineData checks that consecutive data lines of one event are joined with
// newlines, as the SSE format specifies, and that the event type resets after each event.
func TestReadSSEJoinsMultiLineData(t *testing.T) {
	stream := "event: a\ndata: one\ndata: two\n\ndata:three\n\n"
	type ev struct{ typ, data string }
	var got []ev
	err := ReadSSE(strings.NewReader(stream), func(typ, data string) error {
		got = append(got, ev{typ, data})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ev{{"a", "one\ntwo"}, {"", "three"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// TestParseRateLimitHintEdgeCases covers hints whose duration text cannot be summed.
func TestParseRateLimitHintEdgeCases(t *testing.T) {
	tests := []struct {
		body string
		want time.Duration
	}{
		{"rate limited, retry in 5m, ", 5 * time.Minute}, // trailing separator
		{"rate limited, retry in 1h, 2m and 3s.", time.Hour + 2*time.Minute + 3*time.Second},
		{"rate limited, retry in 5 minutes2 s", 0}, // a unit glued to the next number is not a duration
		{"quota exceeded, retry in 25h", 0},        // longer than a day is not believed
	}
	for _, tc := range tests {
		if got := ParseRateLimitHint(tc.body); got != tc.want {
			t.Errorf("ParseRateLimitHint(%q) = %s, want %s", tc.body, got, tc.want)
		}
	}
}

// TestTruncateErrorBodyZeroBudget checks that a zero rune budget shows nothing.
func TestTruncateErrorBodyZeroBudget(t *testing.T) {
	if got := truncateErrorBody([]byte("secret details"), 0); got != "" {
		t.Errorf("truncateErrorBody(_, 0) = %q, want empty", got)
	}
}
