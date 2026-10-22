package chat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hmj-agent/internal/logging"
)

type RetryConfig struct {
	MaxRetries     int           `json:"maxRetries"`
	InitialBackoff time.Duration `json:"initialBackoff"`
	MaxBackoff     time.Duration `json:"maxBackoff"`
	BackoffFactor  float64       `json:"backoffFactor"`
	RetryBuffer    time.Duration `json:"retryBuffer,omitempty"`
}

func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 1 * time.Second,
		MaxBackoff:     15 * time.Second,
		BackoffFactor:  2.0,
		RetryBuffer:    2 * time.Second,
	}
}

type RetryCallback func(attempt int, maxRetries int, delay time.Duration, err error)

// overloadedStatus is the non-standard 529 status Anthropic returns when its API is overloaded.
const overloadedStatus = 529

// IsRetryableStatusCode reports whether an HTTP status warrants a retry.
func IsRetryableStatusCode(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusRequestTimeout,      // 408
		http.StatusConflict,            // 409 (Anthropic: conflict_error; the official SDKs retry it)
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		overloadedStatus:               // 529 (Anthropic: API overloaded)
		return true
	default:
		return false
	}
}

// MaxErrorResponseBytes caps how much of a non-2xx response body is read to build an error.
const MaxErrorResponseBytes = 1 << 20

// StatusError is a non-2xx HTTP response from a provider. Its message keeps the
// "HTTP <code>: <body>" form; callers can match Code to suggest a fix.
type StatusError struct {
	Code int
	// Body is the truncated response body, or a note that it could not be read. An Anthropic
	// error body is shown as "<type>: <message>" instead of its raw JSON.
	Body string
	// Type is the Anthropic error type, such as "billing_error", when the body had that shape.
	Type string
	// RequestID identifies the request to the provider's support, from the body or the
	// request-id header.
	RequestID string
	err       error // the body read failure, when there was one
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body) }

// Unwrap exposes the body read failure, if any.
func (e *StatusError) Unwrap() error { return e.err }

// httpErrorFromResponse turns a non-2xx response into the error and retry
// classification shared by every wire protocol. It consumes the response body.
// When there is no Retry-After header, many providers (e.g. Hyper/Charm, Gemini)
// embed a human-readable wait hint in the error body instead, such as "Please
// try again in 4m39s"; that hint is honored as the reset time.
func httpErrorFromResponse(resp *http.Response) (retryAfter time.Duration, retryable bool, err error) {
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxErrorResponseBytes))
	statusErr := &StatusError{Code: resp.StatusCode}
	if readErr != nil {
		statusErr.Body = fmt.Sprintf("(failed to read error response: %v)", readErr)
		statusErr.err = readErr
	} else {
		shown := data
		if apiErr, ok := parseAnthropicErrorBody(data); ok {
			statusErr.Type = apiErr.Error.Type
			statusErr.RequestID = apiErr.RequestID
			shown = []byte(apiErr.Error.Type + ": " + apiErr.Error.Message)
		}
		statusErr.Body = truncateErrorBody(shown, maxShownErrorRunes)
		// The display is cut short; the diagnostic log keeps the whole body.
		logging.L().Debug("http error body", "status", resp.StatusCode, "body", string(data))
	}
	if statusErr.RequestID == "" {
		statusErr.RequestID = resp.Header.Get("request-id")
	}
	if statusErr.RequestID != "" {
		statusErr.Body += " (request_id " + statusErr.RequestID + ")"
	}
	if loc := resp.Header.Get("Location"); loc != "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		statusErr.Body = truncateErrorBody([]byte("redirect to "+loc+" not followed, so the API key is not sent on; set the provider's baseUrl to the final address"), maxShownErrorRunes)
	}
	err = statusErr
	retryAfter = ParseRetryAfterHeader(resp.Header.Get("Retry-After"))
	if retryAfter <= 0 && len(data) > 0 {
		retryAfter = ParseRateLimitHint(string(data))
	}
	return retryAfter, IsRetryableStatusCode(resp.StatusCode), err
}

// anthropicErrorBody is the error shape of the Anthropic API: {"type":"error","error":{"type",
// "message"},"request_id"}.
type anthropicErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// parseAnthropicErrorBody decodes an Anthropic error body. The top-level "type": "error" tells it
// apart from the OpenAI and Gemini error bodies, which also nest an "error" object.
func parseAnthropicErrorBody(data []byte) (anthropicErrorBody, bool) {
	var b anthropicErrorBody
	if err := json.Unmarshal(data, &b); err != nil || b.Type != "error" || b.Error.Type == "" || b.Error.Message == "" {
		return anthropicErrorBody{}, false
	}
	return b, true
}

// ParseRetryAfterHeader parses a numeric Retry-After header or HTTP date into a duration.
func ParseRetryAfterHeader(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// ParseRateLimitHint extracts a wait duration from a human-readable rate limit
// error body, such as Hyper/Charm's: "Please try again in 4m39s" or
// "You have exceeded your quota, retry in 1h2m3s". Spelled-out units are also
// handled: "retry in 90 seconds", "try again in 2 hours and 15 mins".
func ParseRateLimitHint(body string) time.Duration {
	if body == "" {
		return 0
	}
	// Find "in <duration>" following a try/retry phrase to avoid matching
	// unrelated text like "interest in".
	if !rateLimitHintRe.MatchString(body) {
		return 0
	}
	m := rateLimitInRe.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	// Normalize the captured duration text into something time.ParseDuration
	// accepts: convert spelled-out unit words to abbreviations ("seconds" ->
	// "s", "hours" -> "h"), drop conjunctions such as "and" or "," between
	// compound segments ("2 hours and 15 mins" -> "2h 15m"), and collapse any
	// remaining spaces between numbers and units ("90 s" -> "90s").
	lower := strings.ToLower(m[1])
	lower = durationWordRe.ReplaceAllStringFunc(lower, func(w string) string {
		return unitWordAbbrev[strings.TrimSuffix(strings.TrimSpace(w), "s")]
	})
	lower = durationConjunctionRe.ReplaceAllString(lower, " ")
	normalized := unitSpaceRe.ReplaceAllString(lower, "$1")
	// Accumulate segment-by-segment with a small scanner rather than parsing
	// the whole string, so ordering inside the hint does not matter ("15m2h"
	// still sums correctly) and compound units ("4m39s") split cleanly.
	var total time.Duration
	i := 0
	for i < len(normalized) {
		// Skip whitespace between compound segments ("2h 15m").
		for i < len(normalized) && normalized[i] == ' ' {
			i++
		}
		if i >= len(normalized) {
			break
		}
		// Number part (integer or decimal).
		numStart := i
		for i < len(normalized) && (normalized[i] >= '0' && normalized[i] <= '9' || normalized[i] == '.') {
			i++
		}
		if numStart == i {
			return 0
		}
		// Unit part (one or more letters).
		unitStart := i
		for i < len(normalized) && normalized[i] >= 'a' && normalized[i] <= 'z' {
			i++
		}
		if unitStart == i {
			return 0
		}
		d, err := time.ParseDuration(normalized[numStart:i])
		if err != nil {
			return 0
		}
		total += d
	}
	if total <= 0 || total > maxRateLimitHint {
		return 0
	}
	return total
}

var (
	// Matches "try again in 4m39s", "retry in 1h2m3s", "retry after 90 seconds",
	// "try again in 2 hours and 15 mins", "retry in 90 secs". The trailing
	// non-letter requirement after the whole capture rejects partial matches
	// inside longer words (so "months" is never read as minutes) while still
	// allowing compound abbreviations where a digit follows a unit ("4m39s").
	// RE2 has no lookahead, so a character class + "$" alternative is used.
	rateLimitInRe = regexp.MustCompile(`(?i)\b(?:try again|retry|try)\s+(?:again\s+)?(?:in|after)\s+((?:[0-9]+(?:\.[0-9]+)?\s*(?:milliseconds?|ms|seconds?|secs?|s|minutes?|mins?|m|hours?|h)(?:\s*(?:,|and)\s*)?)+)(?:[^a-zA-Z]|$)`)
	// Matches spelled-out duration unit words (longest alternatives first so
	// "seconds" is consumed whole rather than leaving a bare trailing "s").
	durationWordRe = regexp.MustCompile(`(?i)(milliseconds?|seconds?|secs?|minutes?|mins?|hours?)\b`)
	// Maps a de-pluralized unit word to its Go duration abbreviation.
	unitWordAbbrev = map[string]string{
		"millisecond": "ms",
		"second":      "s",
		"sec":         "s",
		"minute":      "m",
		"min":         "m",
		"hour":        "h",
	}
	// Drops conjunctions between compound duration segments ("2h and 15m").
	durationConjunctionRe = regexp.MustCompile(`(?i)\s*(?:,|and)\s*`)
	// Collapses whitespace between a number and its (abbreviated) unit.
	unitSpaceRe = regexp.MustCompile(`\s*(ms|s|m|h)\b`)
	// Broader gate: only attempt duration extraction on rate-limit-like bodies.
	rateLimitHintRe = regexp.MustCompile(`(?i)rate\s*limit|too many requests|quota|429|try again|retry`)
	// Ignore absurd hints (e.g. malformed text accidentally matching).
	maxRateLimitHint = 24 * time.Hour
	// Small grace period added on top of server-provided wait hints so the
	// retry lands just after the quota reset, not a hair before it.
	retryAfterBuffer = 2 * time.Second
)

func IsRetryableNetError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "timeout")
}

// maxShownErrorRunes bounds the part of a provider's error response shown to the user and
// recorded in the conversation.
const maxShownErrorRunes = 1000

// truncateErrorBody returns a rune-safe string representation of error response bodies,
// bounded to maxRunes to protect prompt context and terminal output from huge HTML/error dumps.
func truncateErrorBody(data []byte, maxRunes int) string {
	s := strings.TrimSpace(string(data))
	if maxRunes <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i] + " [error body truncated]"
		}
		count++
	}
	return s
}
