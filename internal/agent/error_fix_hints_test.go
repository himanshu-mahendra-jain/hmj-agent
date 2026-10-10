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
		{"forbidden wrapped", fmt.Errorf("stream: %w", &chat.StatusError{Code: 403}), "check the provider apiKey / its environment variable"},
		{"not found", &chat.StatusError{Code: 404, Body: "no model"}, "check the model id and baseUrl"},
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
