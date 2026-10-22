package chat

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsContextOverflow(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"Anthropic", &StatusError{Code: 400, Body: "invalid_request_error: prompt is too long: 1200054 tokens > 1000000 maximum"}, true},
		{"OpenAI Responses", &StatusError{Code: 400, Body: `{"error":{"message":"Your input exceeds the context window of this model.","code":"context_length_exceeded"}}`}, true},
		{"OpenAI Chat Completions", &StatusError{Code: 400, Body: `{"error":{"message":"This model's maximum context length is 128000 tokens."}}`}, true},
		{"Gemini", &StatusError{Code: 400, Body: `{"error":{"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576)."}}`}, true},
		{"HTTP 413", &StatusError{Code: http.StatusRequestEntityTooLarge, Body: "request_too_large: Request exceeds the maximum size"}, true},
		{"wrapped", fmt.Errorf("step: %w", &StatusError{Code: 400, Body: "Prompt is too long"}), true},
		{"other 400", &StatusError{Code: 400, Body: "invalid_request_error: tools.0.name: invalid"}, false},
		{"marker on a 500", &StatusError{Code: 500, Body: "prompt is too long"}, false},
		{"not an HTTP error", errors.New("prompt is too long"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := IsContextOverflow(tc.err); got != tc.want {
			t.Errorf("%s: IsContextOverflow = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The error built from a real Anthropic 400 body is recognized, as the API words it.
func TestIsContextOverflowOnAnAnthropicResponse(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Request-Id": {"req_011CgHhUsPCENWa6Z3FtNXNf"}},
		Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error",` +
			`"message":"prompt is too long: 1200054 tokens > 1000000 maximum"},"request_id":"req_011CgHhUsPCENWa6Z3FtNXNf"}`)),
	}
	_, retryable, err := httpErrorFromResponse(resp)
	if retryable || !IsContextOverflow(err) {
		t.Errorf("error %v (retryable %v) is not taken for a context overflow", err, retryable)
	}
}
