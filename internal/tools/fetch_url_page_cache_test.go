package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cacheTestTool returns a fetch_url tool whose every connection goes to srv, so a test can
// fetch public-looking hosts such as docs.example.test, which the cache keeps. The cache and
// its clock are reset when the test ends.
func cacheTestTool(t *testing.T, srv *httptest.Server) *FetchURLTool {
	t.Helper()
	t.Cleanup(func() {
		pageCacheMu.Lock()
		clear(pageCache)
		pageCacheMu.Unlock()
		cacheNow = time.Now
	})
	addr := srv.Listener.Addr().String()
	return &FetchURLTool{Client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}}
}

// countingServer answers with the request count, so a test sees whether a fetch reached it.
func countingServer(t *testing.T, contentType string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		fmt.Fprintf(w, "response %d", n.Add(1))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func run(t *testing.T, tool *FetchURLTool, args string) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A second fetch of a public page within fetchCacheTTL is answered from the first, with its
// age; after that the page is fetched again.
func TestFetchURLCachesPublicPages(t *testing.T) {
	srv, requests := countingServer(t, "text/plain")
	tool := cacheTestTool(t, srv)
	start := time.Now()
	cacheNow = func() time.Time { return start }
	args := `{"url":"http://docs.example.test/page"}`

	if out := run(t, tool, args); out != "response 1" {
		t.Fatalf("first fetch got %q", out)
	}
	if out := run(t, tool, args); out != "[a copy fetched less than a minute ago]\n\nresponse 1" {
		t.Errorf("second fetch got %q", out)
	}
	cacheNow = func() time.Time { return start.Add(5 * time.Minute) }
	if out := run(t, tool, `{"url":"http://docs.example.test/page","max_lines":5}`); out != "[a copy fetched 5 min ago]\n\nresponse 1" {
		t.Errorf("fetch at 5 min got %q", out)
	}
	cacheNow = func() time.Time { return start.Add(fetchCacheTTL) }
	if out := run(t, tool, args); out != "response 2" {
		t.Errorf("fetch after the TTL got %q", out)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests reached the server, want 2", n)
	}
}

// The notes on how a page was made are kept with it.
func TestFetchURLCacheKeepsTheNotes(t *testing.T) {
	srv, _ := countingServer(t, "text/markdown")
	tool := cacheTestTool(t, srv)

	run(t, tool, `{"url":"http://docs.example.test/guide.md"}`)
	if out := run(t, tool, `{"url":"http://docs.example.test/guide.md"}`); out != "[the site served Markdown; a copy fetched less than a minute ago]\n\nresponse 1" {
		t.Errorf("got %q", out)
	}
}

// Pages on this machine or the local network, and error responses, are fetched every time.
func TestFetchURLDoesNotCacheLocalPagesOrErrors(t *testing.T) {
	srv, requests := countingServer(t, "text/plain")
	cacheTestTool(t, srv) // resets the cache afterwards
	for range 2 {
		fetch(t, `{"url":"`+srv.URL+`"}`)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("%d requests to a loopback server, want 2", n)
	}

	var failing atomic.Bool
	failing.Store(true)
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Swap(false) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ready")
	}))
	defer flaky.Close()
	tool := cacheTestTool(t, flaky)
	if out := run(t, tool, `{"url":"http://api.example.test/"}`); !strings.Contains(out, "503") {
		t.Fatalf("first fetch got %q", out)
	}
	if out := run(t, tool, `{"url":"http://api.example.test/"}`); out != "ready" {
		t.Errorf("an error response was cached: got %q", out)
	}
}

func TestIsLocalHost(t *testing.T) {
	local := []string{"localhost", "LOCALHOST.", "127.0.0.1", "::1", "10.1.2.3", "192.168.0.5", "172.16.0.1", "169.254.1.1", "fe80::1", "0.0.0.0", "devbox", "app.localhost", "printer.local", "svc.internal"}
	public := []string{"example.com", "docs.example.test", "8.8.8.8", "2001:4860:4860::8888", "raw.githubusercontent.com"}
	for _, h := range local {
		if !isLocalHost(h) {
			t.Errorf("isLocalHost(%q) = false", h)
		}
	}
	for _, h := range public {
		if isLocalHost(h) {
			t.Errorf("isLocalHost(%q) = true", h)
		}
	}
}

// The cache keeps at most maxCachedPages pages, dropping the oldest first.
func TestFetchCacheDropsTheOldestPages(t *testing.T) {
	srv, _ := countingServer(t, "text/plain")
	cacheTestTool(t, srv) // resets the cache afterwards
	start := time.Now()
	for i := range maxCachedPages + 3 {
		cacheNow = func() time.Time { return start.Add(time.Duration(i) * time.Second) }
		u := mustParse(t, fmt.Sprintf("https://site%d.example.test/", i))
		storeFetch(u, "text", nil)
	}
	if len(pageCache) != maxCachedPages {
		t.Fatalf("%d pages cached, want %d", len(pageCache), maxCachedPages)
	}
	for i := range 3 {
		if _, ok := cachedFetch(fmt.Sprintf("https://site%d.example.test/", i)); ok {
			t.Errorf("page %d, among the oldest, is still cached", i)
		}
	}
	if _, ok := cachedFetch(fmt.Sprintf("https://site%d.example.test/", maxCachedPages+2)); !ok {
		t.Error("the newest page is not cached")
	}
}

// The cache holds at most maxCachedBytes of text, dropping the oldest pages first, and keeps
// no page larger than that.
func TestFetchCacheBoundsItsSize(t *testing.T) {
	srv, _ := countingServer(t, "text/plain")
	cacheTestTool(t, srv)
	start := time.Now()
	third := strings.Repeat("x", maxCachedBytes/3)
	for i := range 4 {
		cacheNow = func() time.Time { return start.Add(time.Duration(i) * time.Second) }
		storeFetch(mustParse(t, fmt.Sprintf("https://site%d.example.test/", i)), third, nil)
	}
	if _, ok := cachedFetch("https://site0.example.test/"); ok {
		t.Error("the oldest page is kept over the size limit")
	}
	if _, ok := cachedFetch("https://site3.example.test/"); !ok {
		t.Error("the newest page is not cached")
	}

	storeFetch(mustParse(t, "https://huge.example.test/"), strings.Repeat("x", maxCachedBytes+1), nil)
	if _, ok := cachedFetch("https://huge.example.test/"); ok {
		t.Error("a page over the whole limit is cached")
	}
}

// Storing a page frees every page that has expired, not only the oldest ones over the limits.
func TestStoringAPageFreesExpiredPages(t *testing.T) {
	srv, _ := countingServer(t, "text/plain")
	cacheTestTool(t, srv)
	start := time.Now()
	cacheNow = func() time.Time { return start }
	storeFetch(mustParse(t, "https://old.example.test/"), "old", nil)
	cacheNow = func() time.Time { return start.Add(fetchCacheTTL) }
	storeFetch(mustParse(t, "https://new.example.test/"), "new", nil)
	if _, ok := pageCache["https://old.example.test/"]; ok {
		t.Error("an expired page is still held after another was stored")
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
