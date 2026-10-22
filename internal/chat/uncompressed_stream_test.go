package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"hmj-agent/internal/config"
)

// TestModelRequestsAskForNoCompression checks that model requests do not ask for gzip, which Go
// otherwise adds to every request.
func TestModelRequestsAskForNoCompression(t *testing.T) {
	for _, tc := range []struct{ api, sse string }{
		{config.APIAnthropic, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		{config.APIResponses, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"},
	} {
		t.Run(tc.api, func(t *testing.T) {
			got := "unset"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Accept-Encoding")
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.sse)
			}))
			defer server.Close()
			cfg := config.Config{Providers: map[string]config.Provider{
				"p": {BaseURL: server.URL, API: tc.api, Models: []config.Model{{ID: "m"}}},
			}}
			if _, _, err := NewClient(cfg).StreamChat(context.Background(), config.ModelRef{Provider: "p", Model: "m"}, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if got != "" {
				t.Errorf("Accept-Encoding = %q, want none", got)
			}
		})
	}
}
