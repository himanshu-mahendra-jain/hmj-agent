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

// searchBackends serves the three web_search backends from one server: the DuckDuckGo
// HTML form at /ddg, the instant answer API at /instant and Wikipedia at /wiki. A nil
// handler answers with an empty but well-formed response.
type searchBackends struct {
	ddg, instant, wiki http.HandlerFunc
}

func (b searchBackends) tool(t *testing.T) *WebSearchTool {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch h := map[string]http.HandlerFunc{"/ddg": b.ddg, "/instant": b.instant, "/wiki": b.wiki}[r.URL.Path]; {
		case h != nil:
			h(w, r)
		case r.URL.Path == "/wiki":
			fmt.Fprint(w, `["q",[],[],[]]`)
		case r.URL.Path == "/instant":
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &WebSearchTool{
		Client:             srv.Client(),
		DDGHTMLEndpoint:    srv.URL + "/ddg",
		DDGInstantEndpoint: srv.URL + "/instant?q=%s",
		WikipediaEndpoint:  srv.URL + "/wiki?search=%s&limit=%d",
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
				instant: reply(`not json`),
				wiki:    reply(`{"not":"an array"}`),
			},
			query: `{"query":"q"}`,
			want:  []string{"all backends failed: duckduckgo: DDG bot challenge encountered; duckduckgo_instant: ", "; wikipedia: unexpected Wikipedia response format"},
		},
		{
			name:     "a truncated DDG page, an empty instant answer and a mistyped Wikipedia array all fail",
			backends: searchBackends{ddg: truncatedBody, instant: status(http.StatusInternalServerError), wiki: reply(`["q","titles",[],[]]`)},
			query:    `{"query":"q"}`,
			want:     []string{"duckduckgo: unexpected EOF", "wikipedia: failed parsing Wikipedia array"},
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
			backends: searchBackends{wiki: reply(`["q",["A","B","C"],["a","b","c"],["https://a","https://b","https://c"]]`)},
			query:    `{"query":"q","limit":1}`,
			want:     []string{"1. [A](https://a)\n   a"},
			wantNot:  []string{"[B]"},
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

// Wikipedia rows whose title or URL is not a string are skipped, and so is a title past
// the end of the URL list, without hiding the good rows in between.
func TestWebSearchWikipediaMalformedRows(t *testing.T) {
	tool := searchBackends{wiki: reply(`["q",[1,"Bad link","Good","No URL"],["","","fine"],["https://one",2,"https://good"]]`)}.tool(t)
	out, err := tool.Execute(context.Background(), `{"query":"q"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1. [Good](https://good)\n   fine") || strings.Contains(out, "Bad link") || strings.Contains(out, "No URL") || strings.Contains(out, "2. ") {
		t.Errorf("output:\n%s", out)
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
		DDGInstantEndpoint: "http://127.0.0.1:1/?q=%s",
		WikipediaEndpoint:  "http://127.0.0.1:1/?s=%s&l=%d",
	}
	out, err := tool.Execute(context.Background(), `{"query":"q"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"all backends failed", "duckduckgo: parse", "duckduckgo_instant: Get", "wikipedia: Get"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// Without overrides the tool talks to the real public endpoints, over HTTPS.
func TestWebSearchDefaultEndpoints(t *testing.T) {
	tool := &WebSearchTool{}
	for _, ep := range []string{tool.ddgHTMLEndpoint(), tool.ddgInstantEndpoint(), tool.wikipediaEndpoint()} {
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
