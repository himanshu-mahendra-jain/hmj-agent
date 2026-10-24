package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// searchBackends serves the web_search backends from one server: the DuckDuckGo HTML form at
// /ddg, Brave at /brave, Bing at /bing, the instant answer API at /instant and Wikipedia at
// /wiki. A nil handler answers with an empty but well-formed response.
type searchBackends struct {
	ddg, brave, bing, instant, wiki http.HandlerFunc
}

func (b searchBackends) tool(t *testing.T) *WebSearchTool {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch h := map[string]http.HandlerFunc{"/ddg": b.ddg, "/brave": b.brave, "/bing": b.bing, "/instant": b.instant, "/wiki": b.wiki}[r.URL.Path]; {
		case h != nil:
			h(w, r)
		case r.URL.Path == "/wiki":
			fmt.Fprint(w, `{"query":{"search":[]}}`)
		case r.URL.Path == "/instant":
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `<html></html>`)
		}
	}))
	t.Cleanup(srv.Close)
	return &WebSearchTool{
		Client:             srv.Client(),
		DDGHTMLEndpoint:    srv.URL + "/ddg",
		BraveEndpoint:      srv.URL + "/brave",
		BingEndpoint:       srv.URL + "/bing",
		DDGInstantEndpoint: srv.URL + "/instant",
		WikipediaEndpoint:  srv.URL + "/wiki",
	}
}

func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

// truncatedBody promises more body than it sends, so reading it fails.
func truncatedBody(w http.ResponseWriter, r *http.Request) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprint(buf, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n<html>")
	buf.Flush()
}

const ddgThreeResults = `<a class="result__a" href="https://one.example/">One</a><a class="result__snippet">first</a>
<a class="result__a" href="https://two.example/">Two</a><a class="result__snippet">second</a>
<a class="result__a" href="https://three.example/">Three</a>`

// web_search falls through its backends in order, keeps to the limit, skips malformed
// rows, and says which backends failed when none produced results.
func TestWebSearchBackendFallthrough(t *testing.T) {
	tests := []struct {
		name     string
		backends searchBackends
		query    string
		want     []string
		wantNot  []string
	}{
		{
			name:     "DDG results are capped at the limit and plain hrefs kept",
			backends: searchBackends{ddg: reply(ddgThreeResults)},
			query:    `{"query":"q","limit":2}`,
			want:     []string{"1. [One](https://one.example/)\n   first", "2. [Two](https://two.example/)\n   second"},
			wantNot:  []string{"Three"},
		},
		{
			name: "instant related topics fill in after a DDG error status",
			backends: searchBackends{
				ddg:     status(http.StatusServiceUnavailable),
				instant: reply(`{"RelatedTopics":[{"Text":"Topic A","FirstURL":"https://a.example"},{"Text":"","FirstURL":"https://skip.example"},{"Text":"Topic B","FirstURL":"https://b.example"},{"Text":"Topic C","FirstURL":"https://c.example"}]}`),
			},
			query:   `{"query":"q","limit":2}`,
			want:    []string{"1. [Topic A](https://a.example)", "2. [Topic B](https://b.example)"},
			wantNot: []string{"Topic C", "skip.example"},
		},
		{
			name: "every backend failing is reported per backend",
			backends: searchBackends{
				ddg:     reply(`<div id="anomaly-modal">are you a robot</div>`),
				brave:   reply(`<html>captcha</html>`),
				bing:    status(http.StatusTooManyRequests),
				instant: reply(`not json`),
				wiki:    reply(`{"not":"a search"}`),
			},
			query: `{"query":"q"}`,
			want:  []string{"all backends failed: duckduckgo: DDG bot challenge encountered; brave: bot challenge; bing: status 429; duckduckgo_instant: ", "; wikipedia: unexpected Wikipedia response format"},
		},
		{
			name:     "a truncated page, an error status and a mistyped Wikipedia answer all fail",
			backends: searchBackends{ddg: truncatedBody, brave: status(http.StatusForbidden), bing: truncatedBody, instant: status(http.StatusInternalServerError), wiki: reply(`{"query":{"search":"titles"}}`)},
			query:    `{"query":"q"}`,
			want:     []string{"duckduckgo: unexpected EOF", "brave: status 403", "bing: unexpected EOF", "wikipedia: unexpected Wikipedia response format"},
		},
		{
			name:     "DuckDuckGo's 202 is a bot challenge, and Brave answers",
			backends: searchBackends{ddg: status(http.StatusAccepted), brave: reply(braveResult("https://b.example/", "From Brave", "Jan 2, 2026 -", "brave text"))},
			query:    `{"query":"q"}`,
			want:     []string{"1. [From Brave](https://b.example/)\n   Jan 2, 2026 · brave text"},
		},
		{
			name:     "empty answers everywhere are not failures",
			backends: searchBackends{ddg: reply(`<html>nothing</html>`)},
			query:    `{"query":"q"}`,
			want:     []string{`No search results found for "q". Try refining`},
			wantNot:  []string{"backends failed"},
		},
		{
			name:     "Wikipedia keeps to the limit",
			backends: searchBackends{wiki: reply(`{"query":{"search":[{"title":"A b","snippet":"<span class=\"searchmatch\">a</span> text","timestamp":"2026-01-02T03:04:05Z"},{"title":"B","snippet":"b"}]}}`)},
			query:    `{"query":"q","limit":1}`,
			want:     []string{"1. [A b](https://en.wikipedia.org/wiki/A_b)\n   updated 2026-01-02 · a text"},
			wantNot:  []string{"[B]"},
		},
		{
			name:     "a Wikipedia title keeps its parentheses, and no date shows without a timestamp",
			backends: searchBackends{wiki: reply(`{"query":{"search":[{"title":"Go (game)","snippet":"board game"}]}}`)},
			query:    `{"query":"q"}`,
			want:     []string{"1. [Go (game)](https://en.wikipedia.org/wiki/Go_(game))\n   board game"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.backends.tool(t).Execute(context.Background(), tc.query)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("output contains %q:\n%s", w, out)
				}
			}
		})
	}
}

// A search cancelled while a fallback backend is answering stops with the context's
// error instead of trying the next backend.
func TestWebSearchCancelledDuringFallback(t *testing.T) {
	for _, backend := range []string{"instant", "wiki"} {
		t.Run(backend, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancelling := func(w http.ResponseWriter, r *http.Request) { cancel(); fmt.Fprint(w, `{}`) }
			b := searchBackends{ddg: status(http.StatusBadGateway)}
			if backend == "instant" {
				b.instant = cancelling
			} else {
				b.wiki = cancelling
			}
			if _, err := b.tool(t).Execute(ctx, `{"query":"q"}`); !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
		})
	}
}

// Endpoints that are not valid URLs, or that refuse connections, count as failed backends.
func TestWebSearchUnusableEndpoints(t *testing.T) {
	tool := &WebSearchTool{
		DDGHTMLEndpoint:    "http://[::1",
		BraveEndpoint:      "http://[::1",
		BingEndpoint:       "http://127.0.0.1:1/",
		DDGInstantEndpoint: "http://127.0.0.1:1/",
		WikipediaEndpoint:  "http://127.0.0.1:1/",
	}
	out, err := tool.Execute(context.Background(), `{"query":"q"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"all backends failed", "duckduckgo: parse", "brave: parse", "bing: Get", "duckduckgo_instant: Get", "wikipedia: Get"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// Without overrides the tool talks to the real public endpoints, over HTTPS.
func TestWebSearchDefaultEndpoints(t *testing.T) {
	tool := &WebSearchTool{}
	for _, ep := range []string{tool.ddgHTMLEndpoint(), tool.braveEndpoint(), tool.bingEndpoint(), tool.ddgInstantEndpoint(), tool.wikipediaEndpoint()} {
		if !strings.HasPrefix(ep, "https://") {
			t.Errorf("endpoint %q is not HTTPS", ep)
		}
	}
}

// The search redirect check stops redirect loops and hosts that do not resolve.
func TestCheckPublicRedirectLimits(t *testing.T) {
	u, _ := url.Parse("https://8.8.8.8/")
	if err := checkPublicRedirect(&http.Request{URL: u}, make([]*http.Request, maxRedirects)); err == nil || !strings.Contains(err.Error(), "stopped after") {
		t.Errorf("redirect loop: err = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // fails either way, offline or not
	defer cancel()
	req := (&http.Request{URL: &url.URL{Scheme: "https", Host: "host.invalid"}}).WithContext(ctx)
	if err := checkPublicRedirect(req, nil); err == nil || !strings.Contains(err.Error(), "redirect to host.invalid") {
		t.Errorf("unresolvable host: err = %v", err)
	}
}
