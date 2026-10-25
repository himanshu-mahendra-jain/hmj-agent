package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"hmj-agent/v2/internal/config"
)

const (
	openAIErr    = `{"error":{"message":"input is required","type":"invalid_request_error","param":null,"code":null}}`
	anthropcErr  = `{"type":"error","error":{"type":"invalid_request_error","message":"model is required"}}`
	fastAPIErr   = `{"detail":[{"loc":["body","messages"],"msg":"field required","type":"missing"}]}`
	googleErr    = `{"error":{"code":400,"message":"model is not specified","status":"INVALID_ARGUMENT"}}`
	googleArrErr = `[` + googleErr + `]`
	htmlNotFound = `<!doctype html><html><title>Not Found</title></html>`
)

// writeTestConfig writes a config file with one provider that has no "api" field, in the
// indented style a user would write, and returns its path.
func writeTestConfig(t *testing.T, provider, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	content := fmt.Sprintf(`{
  "providers": {
    %q: {
      "baseUrl": %q,
      "apiKey": "test-key",
      "models": [
        { "id": "gpt-test" }
      ]
    }
  }
}
`, provider, baseURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newDetectClient returns a client that saves detected protocols into the config at path.
func newDetectClient(t *testing.T, configPath string) *Client {
	t.Helper()
	c := NewClient(config.Config{})
	c.ConfigPath = configPath
	return c
}

// fakeRoutes answers a request with a canned response. A missing route is a 404 with HTML.
type fakeRoutes map[string]func(w http.ResponseWriter, r *http.Request)

func jsonReply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// newFakeServer serves routes and counts every request it receives.
func newFakeServer(t *testing.T, routes fakeRoutes) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if h, ok := routes[r.URL.Path]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, htmlNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func defaultProvider(baseURL string) config.Provider {
	return config.Provider{BaseURL: baseURL, APIKey: "test-key", Models: []config.Model{{ID: "gpt-test"}}}
}

func TestDetectWireAPIShapes(t *testing.T) {
	tests := []struct {
		name       string
		routes     fakeRoutes
		wantAPI    string
		wantSource string
		wantErr    bool
	}{
		{
			name:       "openai 400 on responses",
			routes:     fakeRoutes{"/responses": jsonReply(400, openAIErr)},
			wantAPI:    config.APIResponses,
			wantSource: SourceProbed,
		},
		{
			name:       "anthropic 400 on v1 messages",
			routes:     fakeRoutes{"/v1/messages": jsonReply(400, anthropcErr)},
			wantAPI:    config.APIAnthropic,
			wantSource: SourceProbed,
		},
		{
			name:       "fastapi 422 on chat completions",
			routes:     fakeRoutes{"/chat/completions": jsonReply(422, fastAPIErr)},
			wantAPI:    config.APICompletions,
			wantSource: SourceProbed,
		},
		{
			name:       "google 400 on interactions",
			routes:     fakeRoutes{"/v1beta/interactions": jsonReply(400, googleArrErr)},
			wantAPI:    config.APIGemini,
			wantSource: SourceProbed,
		},
		{
			name:       "404 on everything",
			routes:     fakeRoutes{},
			wantAPI:    config.APIResponses,
			wantSource: config.SourceDefault,
		},
		{
			name: "401 with no body is auth failure",
			routes: fakeRoutes{
				"/responses": jsonReply(401, ""), "/chat/completions": jsonReply(401, ""),
				"/v1/messages": jsonReply(401, ""), "/v1beta/interactions": jsonReply(401, ""),
			},
			wantAPI:    config.APIResponses,
			wantSource: config.SourceDefault,
			wantErr:    true,
		},
		{
			name: "html catch-all answering 200",
			routes: fakeRoutes{
				"/responses":           func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>app</html>") },
				"/chat/completions":    func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>app</html>") },
				"/v1/messages":         func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>app</html>") },
				"/v1beta/interactions": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>app</html>") },
			},
			wantAPI:    config.APIResponses,
			wantSource: config.SourceDefault,
		},
		{
			// Live vyceai-style gateway: Responses answers, and the Anthropic route also returns an
			// Anthropic-shaped 400. Responses is tried first and wins.
			name: "responses and anthropic both answer",
			routes: fakeRoutes{
				"/responses":   jsonReply(400, openAIErr),
				"/v1/messages": jsonReply(400, anthropcErr),
			},
			wantAPI:    config.APIResponses,
			wantSource: SourceProbed,
		},
		{
			// Without a Responses route, Anthropic is tried before Chat Completions.
			name: "anthropic and chat completions both answer",
			routes: fakeRoutes{
				"/v1/messages":      jsonReply(400, anthropcErr),
				"/chat/completions": jsonReply(400, openAIErr),
			},
			wantAPI:    config.APIAnthropic,
			wantSource: SourceProbed,
		},
		{
			name: "chat completions and interactions both answer",
			routes: fakeRoutes{
				"/chat/completions":    jsonReply(400, openAIErr),
				"/v1beta/interactions": jsonReply(400, googleErr),
			},
			wantAPI:    config.APICompletions,
			wantSource: SourceProbed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newFakeServer(t, tc.routes)
			c := newDetectClient(t, writeTestConfig(t, "p", srv.URL))
			api, source, err := c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL))
			if api != tc.wantAPI || source != tc.wantSource {
				t.Fatalf("got (%s, %s), want (%s, %s)", api, source, tc.wantAPI, tc.wantSource)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "auth failed, protocol unknown") {
				t.Fatalf("error %q does not say auth failed, protocol unknown", err)
			}
		})
	}
}

// The probes stop at the first match, in the order responses, anthropic, completions, gemini.
func TestDetectWireAPIProbeOrder(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/chat/completions" {
			jsonReply(400, openAIErr)(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newDetectClient(t, writeTestConfig(t, "p", srv.URL))
	if api, _, _ := c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL)); api != config.APICompletions {
		t.Fatalf("api = %s, want completions", api)
	}
	want := []string{"/responses", "/v1/messages", "/chat/completions"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("probed %v, want %v (and no probe after the match)", paths, want)
	}
}

// A found protocol is written into the config as the provider's "api", keeping the rest of the
// file as the user wrote it, so a fresh client reading that config never probes.
func TestDetectWireAPISavesToConfig(t *testing.T) {
	srv, hits := newFakeServer(t, fakeRoutes{"/v1/messages": jsonReply(400, anthropcErr)})
	path := writeTestConfig(t, "my.proxy", srv.URL)
	before, _ := os.ReadFile(path)

	c := newDetectClient(t, path)
	var told []string
	c.OnProtocolDetected = func(provider, api string, saved bool) {
		told = append(told, fmt.Sprintf("%s=%s saved=%v", provider, api, saved))
	}
	if api, _, err := c.DetectWireAPI(context.Background(), "my.proxy", defaultProvider(srv.URL)); api != config.APIAnthropic || err != nil {
		t.Fatalf("got (%s, %v)", api, err)
	}
	if len(told) != 1 || told[0] != "my.proxy=anthropic saved=true" {
		t.Errorf("OnProtocolDetected calls = %v", told)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantFile := strings.Replace(string(before), "\"my.proxy\": {\n", "\"my.proxy\": {\n      \"api\": \"anthropic\",\n", 1)
	if string(after) != wantFile {
		t.Errorf("config after save:\n%s\nwant:\n%s", after, wantFile)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", fi.Mode().Perm())
	}

	// A fresh process reads the saved field and sends nothing.
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewClient(cfg)
	fresh.ConfigPath = path
	probes := hits.Load()
	api, source, err := fresh.DetectWireAPI(context.Background(), "my.proxy", cfg.Providers["my.proxy"])
	if api != config.APIAnthropic || source != config.SourceExplicit || err != nil || hits.Load() != probes {
		t.Errorf("fresh client = (%s, %s, %v) after %d more requests, want anthropic explicit with no requests", api, source, err, hits.Load()-probes)
	}
}

// When the config cannot be updated the detected protocol is still used, and the user is told
// it was not saved.
func TestDetectWireAPISaveFailureStillUsesProtocol(t *testing.T) {
	srv, _ := newFakeServer(t, fakeRoutes{"/responses": jsonReply(400, openAIErr)})
	c := newDetectClient(t, filepath.Join(t.TempDir(), "missing", "config.json"))
	saved := true
	c.OnProtocolDetected = func(_, _ string, s bool) { saved = s }
	api, source, err := c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL))
	if api != config.APIResponses || source != SourceProbed || err != nil || saved {
		t.Errorf("got (%s, %s, %v) saved=%v, want responses probed, not saved", api, source, err, saved)
	}
	// The result is remembered for the process even though it was not saved.
	if api, _, _ := c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL)); api != config.APIResponses {
		t.Errorf("second call = %s", api)
	}
}

// An explicit "api" or a base URL that settles the protocol never sends a request.
func TestDetectWireAPIShortCircuitSendsNoRequest(t *testing.T) {
	srv, hits := newFakeServer(t, fakeRoutes{"/responses": jsonReply(400, openAIErr)})
	c := newDetectClient(t, writeTestConfig(t, "p", srv.URL))
	for name, tc := range map[string]struct {
		provider   config.Provider
		wantAPI    string
		wantSource string
	}{
		"explicit": {config.Provider{BaseURL: srv.URL, API: config.APIAnthropic}, config.APIAnthropic, config.SourceExplicit},
		"url":      {config.Provider{BaseURL: srv.URL + "/chat/completions"}, config.APICompletions, config.SourceURL},
		"gemini":   {config.Provider{BaseURL: "https://generativelanguage.googleapis.com/v1beta"}, config.APIGemini, config.SourceURL},
	} {
		api, source, err := c.DetectWireAPI(context.Background(), name, tc.provider)
		if api != tc.wantAPI || source != tc.wantSource || err != nil {
			t.Errorf("%s: got (%s, %s, %v), want (%s, %s)", name, api, source, err, tc.wantAPI, tc.wantSource)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("short-circuited providers sent %d requests, want 0", n)
	}
}

// An endpoint that matches nothing is probed once per process and nothing is written to
// the config.
func TestDetectWireAPIRemembersNoMatchInProcess(t *testing.T) {
	srv, hits := newFakeServer(t, fakeRoutes{})
	path := writeTestConfig(t, "p", srv.URL)
	before, _ := os.ReadFile(path)
	c := newDetectClient(t, path)
	provider := defaultProvider(srv.URL)

	api, source, err := c.DetectWireAPI(context.Background(), "p", provider)
	if api != config.APIResponses || source != config.SourceDefault || err != nil {
		t.Fatalf("first call = (%s, %s, %v), want the responses default", api, source, err)
	}
	first := hits.Load()
	if first != 4 {
		t.Fatalf("endpoint got %d probes, want one per protocol (4)", first)
	}
	if api, source, err = c.DetectWireAPI(context.Background(), "p", provider); api != config.APIResponses || source != config.SourceDefault || err != nil || hits.Load() != first {
		t.Errorf("second call = (%s, %s, %v) after %d more requests, want the remembered default and no requests", api, source, err, hits.Load()-first)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a no-match result must not change the config file")
	}
}

// Models of one provider share one detection: concurrent calls probe the endpoint once.
func TestDetectWireAPIProbesOncePerProvider(t *testing.T) {
	srv, hits := newFakeServer(t, fakeRoutes{"/responses": jsonReply(400, openAIErr)})
	c := newDetectClient(t, writeTestConfig(t, "p", srv.URL))
	done := make(chan struct{})
	for range 5 {
		go func() {
			defer func() { done <- struct{}{} }()
			c.DetectWireAPI(context.Background(), "p", defaultProvider(srv.URL))
		}()
	}
	for range 5 {
		<-done
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("endpoint got %d probes from 5 concurrent detections, want 1", n)
	}
}

func TestClassifyProbe(t *testing.T) {
	tests := []struct {
		name   string
		api    string
		status int
		ct     string
		body   string
		want   probeOutcome
	}{
		{"404 html is absent", config.APIResponses, 404, "text/html", htmlNotFound, probeAbsent},
		{"200 html is unknown", config.APIResponses, 200, "text/html", "<html>", probeUnknown},
		{"openai 400 json matches responses", config.APIResponses, 400, "application/json", openAIErr, probeMatch},
		{"openai 401 json matches", config.APICompletions, 401, "application/json; charset=utf-8", openAIErr, probeMatch},
		{"openai shape on messages path is unknown", config.APIAnthropic, 400, "application/json", openAIErr, probeUnknown},
		{"anthropic shape on responses path is unknown", config.APIResponses, 400, "application/json", anthropcErr, probeUnknown},
		{"fastapi 422 matches completions", config.APICompletions, 422, "application/json", fastAPIErr, probeMatch},
		{"fastapi body without json type is unknown", config.APICompletions, 422, "text/plain", fastAPIErr, probeUnknown},
		{"openai shape without json type is unknown", config.APIResponses, 400, "text/plain", openAIErr, probeUnknown},
		{"401 with no body is auth", config.APIResponses, 401, "", "", probeAuth},
		{"403 with html is auth", config.APIResponses, 403, "text/html", "<html>", probeAuth},
		{"405 is unknown", config.APIResponses, 405, "application/json", "", probeUnknown},
		{"google shape matches interactions", config.APIGemini, 400, "application/json; charset=UTF-8", googleErr, probeMatch},
		{"google array shape matches interactions", config.APIGemini, 400, "application/json; charset=UTF-8", googleArrErr, probeMatch},
		{"google shape on completions path is unknown", config.APICompletions, 400, "application/json", googleArrErr, probeUnknown},
		{"openai shape on interactions path is unknown", config.APIGemini, 400, "application/json", openAIErr, probeUnknown},
		{"500 with shape is unknown", config.APIResponses, 500, "application/json", openAIErr, probeUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbe(tc.api, tc.status, tc.ct, []byte(tc.body)); got != tc.want {
				t.Fatalf("classifyProbe = %v, want %v", got, tc.want)
			}
		})
	}
}

// The health probe detects the protocol first and then probes the model with it.
func TestProbeModelDetectsProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.TrimSpace(string(body)) == "{}" {
			jsonReply(400, anthropcErr)(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	cfg := config.Config{Providers: map[string]config.Provider{
		"p": {BaseURL: srv.URL, APIKey: "k", Models: []config.Model{{ID: "m"}}},
	}}
	c := NewClient(cfg)
	c.ConfigPath = writeTestConfig(t, "p", srv.URL)
	res := c.ProbeModel(context.Background(), config.ModelRef{Provider: "p", Model: "m"})
	if res.WireAPI != config.APIAnthropic || res.Status != StatusHealthy {
		t.Errorf("ProbeModel = api %s status %v details %q, want anthropic healthy", res.WireAPI, res.Status, res.Details)
	}
}
