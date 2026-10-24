package tools

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// recordRequests returns a handler that notes each request's form, then answers with status.
func recordRequests(mu *sync.Mutex, got map[string]map[string]string, name string, code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		got[name] = map[string]string{"q": r.Form.Get("q"), "df": r.Form.Get("df"), "tf": r.Form.Get("tf"), "filters": r.Form.Get("filters")}
		mu.Unlock()
		w.WriteHeader(code)
	}
}

// TestWebSearchSendsFiltersToEachEngine checks that each search engine receives the domain
// filters as site: operators and the recency in its own form, and that a backend without that
// recency is skipped.
func TestWebSearchSendsFiltersToEachEngine(t *testing.T) {
	var mu sync.Mutex
	got := map[string]map[string]string{}
	b := searchBackends{
		ddg:     recordRequests(&mu, got, "ddg", http.StatusServiceUnavailable),
		brave:   recordRequests(&mu, got, "brave", http.StatusServiceUnavailable),
		bing:    recordRequests(&mu, got, "bing", http.StatusServiceUnavailable),
		instant: recordRequests(&mu, got, "instant", http.StatusServiceUnavailable),
		wiki:    recordRequests(&mu, got, "wiki", http.StatusServiceUnavailable),
	}
	tool := b.tool(t)
	out, err := tool.Execute(context.Background(), `{"query":"ctx","allowed_domains":["https://www.Go.dev/doc","pkg.go.dev"],"blocked_domains":["spam.example"],"recency":"week"}`)
	if err != nil {
		t.Fatal(err)
	}
	const q = "ctx (site:go.dev OR site:pkg.go.dev) -site:spam.example"
	want := map[string]map[string]string{
		"ddg":   {"q": q, "df": "w", "tf": "", "filters": ""},
		"brave": {"q": q, "df": "", "tf": "pw", "filters": ""},
		"bing":  {"q": q, "df": "", "tf": "", "filters": `ex1:"ez2"`},
	}
	for name, w := range want {
		for k, v := range w {
			if got[name][k] != v {
				t.Errorf("%s got %s=%q, want %q", name, k, got[name][k], v)
			}
		}
	}
	for _, skipped := range []string{"instant", "wiki"} {
		if got[skipped] != nil {
			t.Errorf("%s was asked although it cannot filter by recency", skipped)
		}
	}
	if !strings.Contains(out, "all backends failed") {
		t.Errorf("three tried backends failing is not reported as all failing:\n%s", out)
	}

	clear(got)
	if _, err := tool.Execute(context.Background(), `{"query":"ctx","recency":"year"}`); err != nil {
		t.Fatal(err)
	}
	if got["bing"] != nil || got["ddg"]["df"] != "y" || got["brave"]["tf"] != "py" {
		t.Errorf("recency year: got %v; want Bing skipped, df=y and tf=py", got)
	}
}

// TestWebSearchFiltersResultsByDomain checks that results outside the allowed domains, or in a
// blocked one, are dropped even when the engine returns them, and that the limit counts what
// is left.
func TestWebSearchFiltersResultsByDomain(t *testing.T) {
	page := ddgResult("", "https://go.dev/doc/", "Doc", "") +
		ddgResult("", "https://other.example/", "Other", "") +
		ddgResult("", "https://pkg.go.dev/context", "Pkg", "") +
		ddgResult("", "https://blog.go.dev/post", "Blog", "") +
		ddgResult("", "https://www.go.dev/x", "WWW", "") +
		ddgResult("", "https://notgo.dev/", "Lookalike", "")
	tool := searchBackends{ddg: reply(page)}.tool(t)
	out, err := tool.Execute(context.Background(), `{"query":"q","allowed_domains":["go.dev"],"blocked_domains":["blog.go.dev"],"limit":3}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. [Doc]", "2. [Pkg]", "3. [WWW]"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"Other", "Blog", "Lookalike"} {
		if strings.Contains(out, not) {
			t.Errorf("%q passed the filters:\n%s", not, out)
		}
	}
}

// TestWebSearchFilterArguments checks how domains are read and which arguments are refused.
func TestWebSearchFilterArguments(t *testing.T) {
	got, err := normalizeDomains("allowed_domains", []string{"https://www.Docs.Python.org/3/", "*.example.com", "docs.python.org", " "})
	if err != nil || strings.Join(got, ",") != "docs.python.org,example.com" {
		t.Errorf("normalizeDomains = %v, %v", got, err)
	}
	tool := searchBackends{}.tool(t)
	for args, want := range map[string]string{
		`{"query":"q","recency":"hour"}`:                "invalid recency",
		`{"query":"q","allowed_domains":["localhost"]}`: "invalid allowed_domains entry",
		`{"query":"q","blocked_domains":["a b.com"]}`:   "invalid blocked_domains entry",
	} {
		if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}
