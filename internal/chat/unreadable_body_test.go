package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hmj-agent/v2/internal/config"
)

// cutOffServer answers every request with status and a body that ends after sent, short of
// the 100 bytes its Content-Length promises, so reading it fails with an unexpected EOF.
func cutOffServer(t *testing.T, status int, sent string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n%s", status, http.StatusText(status), sent)
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

func probeCutOff(t *testing.T, status int, sent string) ModelHealthResult {
	t.Helper()
	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: cutOffServer(t, status, sent), API: config.APIResponses, Models: []config.Model{{ID: "m"}}},
	}}
	return NewClient(cfg).ProbeModel(context.Background(), config.ModelRef{Provider: "p", Model: "m"})
}

// A 2xx answer whose body ends before its first byte is not healthy.
func TestProbeModelReportsAnUnreadableSuccessBody(t *testing.T) {
	res := probeCutOff(t, http.StatusOK, "")
	if res.Status == StatusHealthy || res.Error == nil {
		t.Errorf("status %s, error %v: want a failure for a body cut off", res.Status, res.Error)
	}
}

// An error answer whose body cannot be read in full says so next to what was read.
func TestProbeModelNotesAnUnreadableErrorBody(t *testing.T) {
	res := probeCutOff(t, http.StatusInternalServerError, `{"err`)
	if !strings.HasPrefix(res.Details, "HTTP 500") || !strings.Contains(res.Details, "could not be read in full") {
		t.Errorf("details = %q, want the status and a note that the body was cut off", res.Details)
	}
}

// A probe whose answer cannot be read is a transport failure, not a classification of the
// part that arrived.
func TestProbeRouteReportsAnUnreadableBody(t *testing.T) {
	url := cutOffServer(t, http.StatusBadRequest, `{"err`)
	outcome, status, err := NewClient(config.Config{}).probeRoute(context.Background(), probeCandidate{api: config.APIAnthropic, url: url + "/v1/messages"}, "")
	if err == nil || outcome != probeUnknown || status != http.StatusBadRequest {
		t.Errorf("probeRoute = (%v, %d, %v), want unknown with the read error", outcome, status, err)
	}
}

// A turn or compaction whose context ends while the protocol is being probed stops there,
// with the context's error, and sends no request of its own.
func TestCancelledDetectionStopsTheRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	t.Cleanup(srv.Close)
	cfg := config.Config{Providers: map[string]config.Provider{"p": {BaseURL: srv.URL, Models: []config.Model{{ID: "m"}}}}}
	ref := config.ModelRef{Provider: "p", Model: "m"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msgs := []Message{{Role: "user", Content: "hi"}}

	if _, _, err := NewClient(cfg).StreamChat(ctx, ref, msgs, nil, func(string) {}, func(string) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("StreamChat = %v, want context.Canceled", err)
	}
	if _, err := NewClient(cfg).Compact(ctx, ref, msgs, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Compact = %v, want context.Canceled", err)
	}
	if requests != 0 {
		t.Errorf("%d requests reached the server", requests)
	}
}
