package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/logging"
)

// TestModelResponseIsLogged checks that the diagnostic log records each model response with its
// status, HTTP version, content encoding, the provider's request ID, the Anthropic organization and
// workspace IDs, and how long the headers took.
func TestModelResponseIsLogged(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "hmj.log")
	closeLog, err := logging.Init(logPath)
	if err != nil {
		t.Fatalf("logging.Init: %v", err)
	}
	t.Cleanup(func() {
		if _, err := logging.Init(""); err != nil {
			t.Errorf("reset logger: %v", err)
		}
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_test123")
		w.Header().Set("anthropic-organization-id", "org-uuid-1")
		w.Header().Set("anthropic-workspace-id", "wrkspc_test1")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, anthropicEndTurn)
	}))
	defer server.Close()
	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: config.APIAnthropic, Models: []config.Model{{ID: "m"}}},
	}}
	if _, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := closeLog(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"msg":"http response"`, `"status":200`, `"proto":"HTTP/1.1"`, `"content_encoding":""`, `"request_id":"req_test123"`, `"organization_id":"org-uuid-1"`, `"workspace_id":"wrkspc_test1"`, `"headers_after":`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log lacks %s:\n%s", want, data)
		}
	}
}
