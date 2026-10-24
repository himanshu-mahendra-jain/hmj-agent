package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/session"
)

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = old; f.Close() }()
	fn()
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

// TestRefusalNoticeAfterDeclinedTurn checks that a turn the provider's safety classifier
// declined ends with a notice saying so, naming the category when one was given, with any
// control characters in the category removed.
func TestRefusalNoticeAfterDeclinedTurn(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		name    string
		details string
		want    string
		mustNot string
	}{
		{"with category", `,"stop_details":{"type":"refusal","category":"cyber\u001b[2J","explanation":"no"}`, "[the model declined this request (safety classifier: cyber", "\x1b[2J"},
		{"without category", ``, "[the model declined this request (safety classifier)]", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
				fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"%s},\"usage\":{\"output_tokens\":1}}\n\n", tc.details)
				fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer srv.Close()
			cfg := config.Config{Providers: map[string]config.Provider{
				"claude": {BaseURL: srv.URL, API: "anthropic", Models: []config.Model{{ID: "m"}}},
			}}
			r := NewRunnerWithTools(chat.NewClient(cfg), cfg, nil)
			conv := &session.Conversation{ID: "1", Model: config.ModelRef{Provider: "claude", Model: "m"}}

			out := captureOutput(t, func() {
				if err := r.Run(context.Background(), conv, "something risky"); err != nil {
					t.Errorf("Run: %v", err)
				}
			})
			if !strings.Contains(out, tc.want) {
				t.Errorf("output lacks %q:\n%q", tc.want, out)
			}
			if tc.mustNot != "" && strings.Contains(out, tc.mustNot) {
				t.Errorf("the category's escape sequence reached the terminal: %q", out)
			}
			if !strings.Contains(out, "tokens:") {
				t.Errorf("a declined turn should still end with the usage footer: %q", out)
			}
		})
	}
}

// TestProtocolDetectedNotice checks the notice for a probed provider protocol, with and
// without the config file being updated.
func TestProtocolDetectedNotice(t *testing.T) {
	cases := []struct {
		saved bool
		want  string
	}{
		{true, `[provider acme speaks the anthropic API; saved "api": "anthropic" to the config file]`},
		{false, `[provider acme speaks the anthropic API; the config file could not be updated, so it will be probed again next run]`},
	}
	for _, tc := range cases {
		if got := ProtocolDetectedNotice("acme", "anthropic", tc.saved); got != tc.want {
			t.Errorf("ProtocolDetectedNotice(saved=%v) = %q, want %q", tc.saved, got, tc.want)
		}
	}
}

// TestRunnerInstallsClientNotices checks that a new runner gives its client the retry and
// protocol-detection notices, which go to stderr, and keeps callbacks the caller set.
func TestRunnerInstallsClientNotices(t *testing.T) {
	client := chat.NewClient(config.Config{})
	NewRunnerWithTools(client, config.Config{}, nil)
	if client.OnRetry == nil || client.OnProtocolDetected == nil {
		t.Fatal("runner did not install the client notices")
	}

	cases := []struct {
		name string
		fire func()
		want *regexp.Regexp
	}{
		{"short retry", func() { client.OnRetry(1, 3, 1500*time.Millisecond, errors.New("503 busy")) }, regexp.MustCompile(`\[retry 1/3 in 1\.5s\]: 503 busy`)},
		{"long retry shows the clock time", func() { client.OnRetry(2, 3, 2*time.Minute, errors.New("rate limited")) }, regexp.MustCompile(`\[retry 2/3 at \d\d:\d\d:\d\d\]: rate limited`)},
		{"protocol detected", func() { client.OnProtocolDetected("acme", "responses", true) }, regexp.MustCompile(`\[provider acme speaks the responses API; saved`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if out := captureStderr(t, tc.fire); !tc.want.MatchString(out) {
				t.Errorf("stderr = %q, want a match for %s", out, tc.want)
			}
		})
	}

	called := false
	own := chat.NewClient(config.Config{})
	own.OnProtocolDetected = func(string, string, bool) { called = true }
	NewRunnerWithTools(own, config.Config{}, nil)
	own.OnProtocolDetected("p", "responses", false)
	if !called {
		t.Error("the runner replaced a protocol callback the caller had set")
	}
}

// TestProviderErrorsCannotDriveTheTerminal checks that the notices quoting a provider's error, whose
// body and headers the server controls, print it without its escape sequences.
func TestProviderErrorsCannotDriveTheTerminal(t *testing.T) {
	providerErr := &chat.StatusError{Code: 503, Body: "busy\x1b]52;c;eA==\x07 now\x1b[2J (request_id req\x1b]0;x\x07)"}
	client := chat.NewClient(config.Config{})
	NewRunnerWithTools(client, config.Config{}, nil)

	for name, out := range map[string]string{
		"retry":      captureStderr(t, func() { client.OnRetry(1, 3, time.Second, providerErr) }),
		"compaction": captureOutput(t, func() { reportCompactionError(providerErr) }),
	} {
		if strings.Contains(out, "\x1b]") || strings.ContainsRune(out, 0x07) || strings.Contains(out, "\x1b[2J") || !strings.Contains(out, "busy now") {
			t.Errorf("%s notice = %q", name, out)
		}
	}
}
