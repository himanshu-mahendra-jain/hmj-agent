package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shortBodyServer answers every request with the given status, promising a 100-byte body
// but closing the connection after a few bytes, so reading the body fails.
func shortBodyServer(t *testing.T, status string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 %s\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\nshort", status)
		buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fetch_url reports HTTP errors and broken responses as results the model can act on,
// and caps max_lines at 1000.
func TestFetchURLResponses(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer notFound.Close()
	manyLines := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := range 1500 {
			fmt.Fprintf(w, "line %d\n", i)
		}
	}))
	defer manyLines.Close()
	brokenError := shortBodyServer(t, "500 Internal Server Error")
	brokenOK := shortBodyServer(t, "200 OK")

	tests := []struct {
		name     string
		args     string
		want     string // substring of the result
		wantErr  string
		wantNot  string
		exactOut bool
	}{
		{name: "status error names the status once", args: `{"url":"` + notFound.URL + `/x"}`, want: "[HTTP 404 Not Found for " + notFound.URL + "/x]", exactOut: true},
		{name: "error body cut short", args: `{"url":"` + brokenError.URL + `"}`, want: "[HTTP 500 Internal Server Error for " + brokenError.URL + "; response drain failed: unexpected EOF]", exactOut: true},
		{name: "success body cut short", args: `{"url":"` + brokenOK.URL + `"}`, wantErr: "failed to read response"},
		{name: "max_lines capped at 1000", args: `{"url":"` + manyLines.URL + `","max_lines":5000}`, want: "[truncated to 1000 lines of 1501 total; use max_lines to view more]", wantNot: "line 1000\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&FetchURLTool{}).Execute(context.Background(), tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("got %q, %v; want error %q", out, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (tc.exactOut && out != tc.want) || !strings.Contains(out, tc.want) {
				t.Errorf("got %q, want %q", tail(out), tc.want)
			}
			if tc.wantNot != "" && strings.Contains(out, tc.wantNot) {
				t.Errorf("output contains %q", tc.wantNot)
			}
		})
	}
}

func tail(s string) string { return s[max(0, len(s)-200):] }
