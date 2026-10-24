package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowServer sends headers and the first part of the body at once, then waits for the client
// to give up; withHeaders false holds back the headers too.
func slowServer(t *testing.T, withHeaders bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if withHeaders {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "partial")
			http.NewResponseController(w).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// timeout_seconds bounds the whole response, headers and body, and the error says how to
// wait longer.
func TestFetchURLTimeoutSeconds(t *testing.T) {
	for _, withHeaders := range []bool{false, true} {
		t.Run(fmt.Sprintf("headers sent %v", withHeaders), func(t *testing.T) {
			t.Parallel()
			srv := slowServer(t, withHeaders)
			start := time.Now()
			_, err := (&FetchURLTool{}).Execute(context.Background(), `{"url":"`+srv.URL+`","timeout_seconds":1}`)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("took %v with timeout_seconds 1", elapsed)
			}
			if err == nil || !strings.Contains(err.Error(), "no complete response within 1s; set timeout_seconds (up to 120) to wait longer") {
				t.Errorf("got error %v", err)
			}
		})
	}
}

// A halt is reported as a cancellation, not as a timeout to raise.
func TestFetchURLHaltIsNotATimeout(t *testing.T) {
	srv := slowServer(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := (&FetchURLTool{}).Execute(ctx, `{"url":"`+srv.URL+`"}`)
	if err == nil || strings.Contains(err.Error(), "timeout_seconds") {
		t.Errorf("got error %v", err)
	}
}

// A redirect to another host is named in the result; one within the same host is not.
func TestFetchURLNotesARedirectToAnotherHost(t *testing.T) {
	target := textServer(t, "moved content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/elsewhere":
			http.Redirect(w, r, target.URL+"/new", http.StatusMovedPermanently)
		case "/old":
			http.Redirect(w, r, "/new", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "same host content")
		}
	}))
	defer srv.Close()

	if out := fetch(t, `{"url":"`+srv.URL+`/elsewhere"}`); out != "[redirected to "+target.URL+"/new]\n\nmoved content" {
		t.Errorf("got %q", out)
	}
	if out := fetch(t, `{"url":"`+srv.URL+`/old"}`); out != "same host content" {
		t.Errorf("got %q", out)
	}
}
