package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"hmj-agent/internal/config"
)

// TestProbeModelFailuresBeforeTheRequest checks the health probe for models it cannot even send a
// request to: an unknown model and an unparseable base URL.
func TestProbeModelFailuresBeforeTheRequest(t *testing.T) {
	t.Run("unknown model", func(t *testing.T) {
		res := clientFor(config.APIResponses, "http://127.0.0.1", 0).ProbeModel(context.Background(), config.ModelRef{Provider: "p", Model: "nope"})
		if res.Status != StatusFailed || res.Error == nil || res.Details != res.Error.Error() {
			t.Errorf("result = %+v, want FAIL with the resolve error as details", res)
		}
	})
	t.Run("invalid base URL", func(t *testing.T) {
		res := clientFor(config.APIResponses, "http://bad host", 0).ProbeModel(context.Background(), testRef)
		if res.Status != StatusFailed || !strings.HasPrefix(res.Details, "request error:") {
			t.Errorf("result = %+v, want FAIL with a request error", res)
		}
	})
}

// TestProbeModelTransportFailures checks how a probe that never gets a response is reported: a
// cancelled context is "cancelled", a refused connection carries the network error.
func TestProbeModelTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	t.Run("connection refused", func(t *testing.T) {
		res := clientFor(config.APIResponses, url, 0).ProbeModel(context.Background(), testRef)
		if res.Status != StatusFailed || !strings.Contains(res.Details, "connection refused") {
			t.Errorf("result = %+v, want FAIL with the connection error", res)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := clientFor(config.APIResponses, url, 0).ProbeModel(ctx, testRef)
		if res.Status != StatusFailed || res.Details != "cancelled" {
			t.Errorf("result = %+v, want FAIL cancelled", res)
		}
	})
}

// TestProbeModelGatewayTimeoutAndEmptyBody checks that a 504 is a timeout and that an error with
// no body is described by its status text.
func TestProbeModelGatewayTimeoutAndEmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer server.Close()

	res := clientFor(config.APIResponses, server.URL, 0).ProbeModel(context.Background(), testRef)
	if res.Status != StatusTimeout || res.StatusCode != http.StatusGatewayTimeout || res.Details != "HTTP 504: Gateway Timeout" {
		t.Errorf("result = %+v, want TIMEOUT with HTTP 504: Gateway Timeout", res)
	}
}

// TestProbeModelAuthFailureDuringDetection checks that when protocol detection is refused with a
// bare 401 on every route, the probe's own failure says the protocol is unknown.
func TestProbeModelAuthFailureDuringDetection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	res := clientFor("", server.URL, 0).ProbeModel(context.Background(), testRef)
	if res.Status != StatusAuthError || !strings.HasPrefix(res.Details, "protocol unknown: HTTP 401") {
		t.Errorf("result = %+v, want AUTH_ERROR prefixed with protocol unknown", res)
	}
}

// TestProbeRequestEndpointSuffixes checks that a base URL already naming the endpoint is probed
// as it is.
func TestProbeRequestEndpointSuffixes(t *testing.T) {
	tests := []struct{ api, base string }{
		{config.APICompletions, "https://h/v1/chat/completions"},
		{config.APICompletions, "https://h/v1/completions"},
		{config.APIAnthropic, "https://h/v1/messages"},
		{config.APIAnthropic, "https://h/proxy/messages"},
		{config.APIResponses, "https://h/v1/responses"},
		{config.APIGemini, "https://h/v1beta/interactions"},
	}
	for _, tc := range tests {
		url, body, err := probeRequest(tc.api, tc.base, config.Model{ID: "m"})
		if err != nil || url != tc.base || len(body) == 0 {
			t.Errorf("probeRequest(%s, %s) = %q, %d bytes, %v; want the base URL unchanged", tc.api, tc.base, url, len(body), err)
		}
	}
}

// TestCheckAllHealthNoModels checks that probing no models returns nothing and reports no progress.
func TestCheckAllHealthNoModels(t *testing.T) {
	called := false
	if got := NewClient(config.Config{}).CheckAllHealth(context.Background(), nil, func(int, int) { called = true }); got != nil || called {
		t.Errorf("CheckAllHealth(nil) = %v (progress called %v), want nil", got, called)
	}
}

// TestCheckAllHealthReportsProgressInOrder checks that every probe reports progress up to the total
// and that results come back in the order the models were given, whatever order they finish in.
func TestCheckAllHealthReportsProgressInOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := NewClient(config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: server.URL, API: config.APIResponses, Models: []config.Model{{ID: "a"}, {ID: "b"}, {ID: "c"}}},
	}})
	refs := []config.ModelRef{{Provider: "p", Model: "c"}, {Provider: "p", Model: "a"}, {Provider: "p", Model: "b"}}

	var mu sync.Mutex
	var progress []int
	results := c.CheckAllHealth(context.Background(), refs, func(done, total int) {
		mu.Lock()
		defer mu.Unlock()
		if total != len(refs) {
			t.Errorf("total = %d, want %d", total, len(refs))
		}
		progress = append(progress, done)
	})
	if fmt.Sprint(progress) != "[1 2 3]" {
		t.Errorf("progress = %v, want [1 2 3]", progress)
	}
	for i, r := range results {
		if r.Ref != refs[i] || r.Status != StatusHealthy {
			t.Errorf("result %d = %+v, want %v healthy", i, r, refs[i])
		}
	}
}

// TestParseHealthErrorMessage checks how a failed probe's body becomes the one-line detail.
func TestParseHealthErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty body uses status text", "", "Too Many Requests"},
		{"message field", `{"message":"slow down"}`, "slow down"},
		{"detail field", `{"detail":"not allowed"}`, "not allowed"},
		{"error object", `{"error":{"message":"bad key"}}`, "bad key"},
		{"error object without message", `{"error":{"code":1}}`, `{"error":{"code":1}}`},
		{"error string", `{"error":"quota"}`, "quota"},
		{"not JSON", "upstream\r\n  is   down", "upstream is down"},
		{"long message truncated", strings.Repeat("x", 80), strings.Repeat("x", 52) + "..."},
		{"key shown as sent", `{"message":"bad key sk-abcdefghijklmnopqrstuvwxyz0123"}`, "bad key sk-abcdefghijklmnopqrstuvwxyz0123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHealthErrorMessage([]byte(tc.body), http.StatusTooManyRequests)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProbeCandidatesKeepEndpointSuffixes checks that a base URL that already names the Responses
// or Chat Completions endpoint is probed there, not with the path doubled.
func TestProbeCandidatesKeepEndpointSuffixes(t *testing.T) {
	for _, base := range []string{"https://h/v1/responses", "https://h/v1/chat/completions/"} {
		found := false
		for _, c := range probeCandidates(base) {
			if c.url == strings.TrimRight(base, "/") {
				found = true
			}
			if strings.Contains(c.url, "/responses/responses") || strings.Contains(c.url, "/completions/chat/completions") {
				t.Errorf("candidate %+v doubles the endpoint path", c)
			}
		}
		if !found {
			t.Errorf("no candidate probes %s itself", base)
		}
	}
}

// TestErrorShapeUnrecognised checks bodies that look like errors but match no known envelope.
func TestErrorShapeUnrecognised(t *testing.T) {
	for _, body := range []string{`[]`, `[1,`, `{"error":{"type":"x"}}`, `{"error":"text"}`, `not json`} {
		if got := errorShape([]byte(body)); got != "" {
			t.Errorf("errorShape(%s) = %q, want none", body, got)
		}
	}
}

// TestDetectWireAPITransportFailuresFallBack checks that an endpoint that cannot be reached falls
// back to responses with no error, since nothing was learned about its protocol.
func TestDetectWireAPITransportFailuresFallBack(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()

	api, source, err := NewClient(config.Config{}).DetectWireAPI(context.Background(), "p", defaultProvider(url))
	if api != config.APIResponses || source != config.SourceDefault || err != nil {
		t.Errorf("DetectWireAPI = %s, %s, %v; want responses, default, nil", api, source, err)
	}

	api, _, err = NewClient(config.Config{}).DetectWireAPI(context.Background(), "p", defaultProvider("http://bad host"))
	if api != config.APIResponses || err != nil {
		t.Errorf("DetectWireAPI(bad URL) = %s, %v; want responses, nil", api, err)
	}
}

// TestDetectWireAPICancelled checks that cancelling detection returns the cancellation and does
// not remember the endpoint as matching nothing.
func TestDetectWireAPICancelled(t *testing.T) {
	srv, hits := newFakeServer(t, fakeRoutes{"/responses": jsonReply(400, openAIErr)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewClient(config.Config{})
	if _, _, err := c.DetectWireAPI(ctx, "p", defaultProvider(srv.URL)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	api, source, err := c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL))
	if api != config.APIResponses || source != SourceProbed || err != nil {
		t.Errorf("after a cancelled detection: %s, %s, %v; want a fresh probe that finds responses", api, source, err)
	}
	if hits.Load() == 0 {
		t.Error("the second detection sent no probe")
	}
}

// TestDetectWireAPIWithoutConfigPath checks that a detected protocol is reported as not saved
// when the client has no config file to write it to.
func TestDetectWireAPIWithoutConfigPath(t *testing.T) {
	srv, _ := newFakeServer(t, fakeRoutes{"/v1/messages": jsonReply(400, anthropcErr)})
	c := NewClient(config.Config{})
	c.ConfigPath = ""
	var gotAPI string
	saved := true
	c.OnProtocolDetected = func(_, api string, s bool) { gotAPI, saved = api, s }
	if api, _, err := c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL)); api != config.APIAnthropic || err != nil {
		t.Fatalf("DetectWireAPI = %s, %v; want anthropic", api, err)
	}
	if gotAPI != config.APIAnthropic || saved {
		t.Errorf("OnProtocolDetected(%q, saved=%v), want anthropic and not saved", gotAPI, saved)
	}
}

// TestDetectWireAPIConcurrentNoMatch checks that concurrent detections of an endpoint that matches
// nothing probe it once and all get the same default answer.
func TestDetectWireAPIConcurrentNoMatch(t *testing.T) {
	release := make(chan struct{})
	srv, hits := newFakeServer(t, fakeRoutes{"/responses": func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the first probe until every detection has started
		w.WriteHeader(http.StatusInternalServerError)
	}})
	c := NewClient(config.Config{})
	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range results {
		wg.Go(func() {
			api, source, _ := c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL))
			results[i] = api + "/" + source
		})
	}
	// The other detections queue behind the first one's lock while its probe is held.
	waitUntilQueued(t, c, srv.URL)
	close(release)
	wg.Wait()
	for i, r := range results {
		if r != config.APIResponses+"/"+config.SourceDefault {
			t.Errorf("detection %d = %s, want responses/default", i, r)
		}
	}
	if n := hits.Load(); n != 4 {
		t.Errorf("endpoint got %d probes, want one round of 4", n)
	}
}

// waitUntilQueued waits until detection of baseURL has started, so the held probe is in flight.
func waitUntilQueued(t *testing.T, c *Client, baseURL string) {
	t.Helper()
	pm := c.protocolMemoryFor()
	key := protocolKey(baseURL)
	for {
		pm.mu.Lock()
		_, ok := pm.keyMu[key]
		pm.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatal("detection never started")
		default:
			runtime.Gosched()
		}
	}
}
