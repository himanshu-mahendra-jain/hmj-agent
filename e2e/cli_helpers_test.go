package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// buildCLI returns the hmj binary TestMain built for these tests.
func buildCLI(t *testing.T) string {
	t.Helper()
	return binPath
}

// cliEnv is the environment for the binary: the test's own environment plus, under
// HMJ_COVER_DIR, the GOCOVERDIR the instrumented binary writes its counters to.
func cliEnv() []string {
	env := cleanEnv()
	if coverDir != "" {
		env = append(env, "GOCOVERDIR="+coverDir)
	}
	return env
}

// mockAgentServer answers every request with chunks as one server-sent event stream.
func mockAgentServer(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
}
