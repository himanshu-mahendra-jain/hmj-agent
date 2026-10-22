package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"testing"

	"hmj-agent/internal/chat"
)

func TestRemediationHint(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"config missing", fmt.Errorf("config: %w", &fs.PathError{Op: "open", Path: "x", Err: os.ErrNotExist}),
			"create ~/.hmj/config.json (see config.example.json) or set HMJ_CONFIG"},
		{"unauthorized", &chat.StatusError{Code: 401, Body: "bad key"}, "check the provider apiKey / its environment variable"},
		{"payment required", &chat.StatusError{Code: 402, Type: "billing_error"}, "check the billing and credit balance of the provider account"},
		{"forbidden wrapped", fmt.Errorf("stream: %w", &chat.StatusError{Code: 403}), "check the provider apiKey, and that its account and workspace may use this model"},
		{"not found", &chat.StatusError{Code: 404, Body: "no model"}, "check the model id and baseUrl"},
		{"too large", &chat.StatusError{Code: 413}, "the request is too large for the provider; start a new conversation with /new"},
		{"server error", &chat.StatusError{Code: 500}, ""},
		{"dial error", fmt.Errorf("request: %w", netErr), "check baseUrl and network connectivity"},
		{"other", errors.New("boom"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RemediationHint(tt.err); got != tt.want {
				t.Errorf("RemediationHint(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
