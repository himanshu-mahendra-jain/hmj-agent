package chat

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRetryAndRateLimitParsing(t *testing.T) {
	// 1. ParseRetryAfterHeader seconds
	if d := ParseRetryAfterHeader("12"); d != 12*time.Second {
		t.Fatalf("expected 12s, got %v", d)
	}
	if d := ParseRetryAfterHeader(""); d != 0 {
		t.Fatalf("expected 0 for empty, got %v", d)
	}
	if d := ParseRetryAfterHeader("invalid"); d != 0 {
		t.Fatalf("expected 0 for invalid, got %v", d)
	}

	// Future HTTP-Date
	future := time.Now().Add(30 * time.Second).UTC().Format(time.RFC1123Z)
	if d := ParseRetryAfterHeader(future); d <= 0 || d > 35*time.Second {
		// try with time.RFC1123 replace UTC with GMT
		futureGMT := time.Now().Add(30 * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
		if d2 := ParseRetryAfterHeader(futureGMT); d2 <= 0 || d2 > 35*time.Second {
			t.Fatalf("unexpected duration for future date: %v (gmt: %v)", d, d2)
		}
	}

	// Past HTTP-Date
	past := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC1123)
	if d := ParseRetryAfterHeader(past); d != 0 {
		t.Fatalf("expected 0 for past date, got %v", d)
	}

	// 2. ParseRateLimitHint bodies
	hints := map[string]time.Duration{
		"":                                      0,
		"no hint here":                          0,
		"Please try again in 4m39s":             4*time.Minute + 39*time.Second,
		"Rate limit exceeded. Retry in 1h2m3s.": 1*time.Hour + 2*time.Minute + 3*time.Second,
		"try again in 90 seconds":               90 * time.Second,
		"retry in 2 hours and 15 mins":          2*time.Hour + 15*time.Minute,
	}

	for body, expected := range hints {
		got := ParseRateLimitHint(body)
		if got != expected {
			t.Errorf("for body %q: expected %v, got %v", body, expected, got)
		}
	}

	// Retry config defaults
	cfg := DefaultRetryConfig()
	if cfg.MaxRetries != 3 || cfg.InitialBackoff != time.Second {
		t.Fatalf("unexpected DefaultRetryConfig: %+v", cfg)
	}

	// Status codes
	if !IsRetryableStatusCode(429) || !IsRetryableStatusCode(503) || IsRetryableStatusCode(200) || IsRetryableStatusCode(400) {
		t.Fatal("unexpected IsRetryableStatusCode results")
	}

	// Retry-After header wrapper
	if ParseRetryAfterHeader("15") != 15*time.Second {
		t.Fatal("unexpected ParseRetryAfterHeader result")
	}
}

type dummyNetError struct{}

func (d dummyNetError) Error() string   { return "dummy net error" }
func (d dummyNetError) Timeout() bool   { return true }
func (d dummyNetError) Temporary() bool { return true }

func TestIsRetryableNetError(t *testing.T) {
	if IsRetryableNetError(nil) {
		t.Fatal("nil error should not be retryable")
	}
	if IsRetryableNetError(context.Canceled) {
		t.Fatal("context.Canceled should not be retryable")
	}
	if IsRetryableNetError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded should not be retryable")
	}
	if !IsRetryableNetError(dummyNetError{}) {
		t.Fatal("net.Error should be retryable")
	}
	if !IsRetryableNetError(io.EOF) {
		t.Fatal("io.EOF should be retryable")
	}
	if !IsRetryableNetError(io.ErrUnexpectedEOF) {
		t.Fatal("io.ErrUnexpectedEOF should be retryable")
	}
	if !IsRetryableNetError(errors.New("connection reset by peer")) {
		t.Fatal("connection reset should be retryable")
	}
	if !IsRetryableNetError(errors.New("broken pipe")) {
		t.Fatal("broken pipe should be retryable")
	}
	if !IsRetryableNetError(errors.New("connection refused")) {
		t.Fatal("connection refused should be retryable")
	}
	if !IsRetryableNetError(errors.New("i/o timeout")) {
		t.Fatal("timeout should be retryable")
	}
	if IsRetryableNetError(errors.New("permission denied")) {
		t.Fatal("permission denied should not be retryable")
	}
}
