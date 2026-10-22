package chat

import (
	"errors"
	"net/http"
	"strings"
)

// contextOverflowMarkers are phrases the providers use when a request's prompt is longer than
// the model's context window, in lower case.
var contextOverflowMarkers = []string{
	"prompt is too long",                    // Anthropic
	"context_length_exceeded",               // OpenAI error code
	"maximum context length",                // OpenAI Chat Completions and compatible servers
	"exceeds the context window",            // OpenAI Responses
	"exceeds the maximum number of tokens",  // Gemini
	"input is too long for requested model", // Amazon Bedrock
}

// IsContextOverflow reports whether err is a provider's refusal of a request whose prompt is too
// long for the model: an HTTP 413, or an HTTP 400 whose body names the context limit.
func IsContextOverflow(err error) bool {
	statusErr, ok := errors.AsType[*StatusError](err)
	if !ok {
		return false
	}
	if statusErr.Code == http.StatusRequestEntityTooLarge {
		return true
	}
	if statusErr.Code != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(statusErr.Body)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}
