package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestProviderRedirectIsNotFollowed checks, for every protocol, that a provider answering
// with a redirect to another host is reported as an error, not retried, and never sent
// the request, so the API key (x-api-key, x-goog-api-key or Authorization) stays put.
func TestProviderRedirectIsNotFollowed(t *testing.T) {
	for _, api := range allAPIs {
		t.Run(api, func(t *testing.T) {
			var elsewhere atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
			defer other.Close()

			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			defer server.Close()

			_, _, err := clientFor(api, server.URL, 3).StreamChat(context.Background(), testRef, nil, nil, nil, nil)
			var se *StatusError
			if !errors.As(err, &se) || se.Code != http.StatusTemporaryRedirect {
				t.Fatalf("err = %v, want a 307 StatusError", err)
			}
			if !strings.Contains(se.Body, "not followed") || !strings.Contains(se.Body, other.URL) {
				t.Errorf("error body = %q, want it to name the redirect target and say it was not followed", se.Body)
			}
			if elsewhere.Load() != 0 {
				t.Errorf("the redirect target got %d requests, want none", elsewhere.Load())
			}
			if hits.Load() != 1 {
				t.Errorf("provider got %d requests, want 1 (a redirect is not retried)", hits.Load())
			}
		})
	}
}
