package tools

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// useSearchClock sets the cache's clock to a fixed time the test can move.
func useSearchClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.Local)
	old := cacheNow
	cacheNow = func() time.Time { return now }
	t.Cleanup(func() { cacheNow = old })
	return &now
}

// TestWebSearchHeaderNamesSourceAndDate checks the header of a result list, which names the
// source that answered and today's date, and the note of the sources that failed before it.
func TestWebSearchHeaderNamesSourceAndDate(t *testing.T) {
	useSearchClock(t)
	tool := searchBackends{
		ddg:   status(http.StatusAccepted),
		brave: reply(braveResult("https://b.example/", "B", "2 days ago -", "text")),
	}.tool(t)
	out, err := tool.Execute(context.Background(), `{"query":"q"}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `Search results for "q" from Brave (today is 2026-10-23):`; !strings.HasPrefix(out, want) {
		t.Errorf("header: got %q, want %q", out[:min(len(out), 80)], want)
	}
	if want := "Sources that failed first: duckduckgo: DDG bot challenge encountered (status 202)\n"; !strings.Contains(out, want) {
		t.Errorf("no %q in:\n%s", want, out)
	}
}

// TestWebSearchCachesAnswers checks that a repeated search is answered from the cache, saying
// so, until fetchCacheTTL passes, and that a search with other filters or limit, or one that
// found nothing, is not.
func TestWebSearchCachesAnswers(t *testing.T) {
	now := useSearchClock(t)
	var requests atomic.Int32
	var found atomic.Bool
	found.Store(true)
	tool := searchBackends{ddg: func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if found.Load() {
			reply(ddgResult("", "https://go.dev/", "Go", "the site"))(w, r)
		}
	}}.tool(t)
	search := func(args string) string {
		t.Helper()
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := search(`{"query":"go"}`)
	*now = now.Add(3 * time.Minute)
	second := search(`{"query":"go"}`)
	if requests.Load() != 1 {
		t.Fatalf("%d requests for a repeated search, want 1", requests.Load())
	}
	if !strings.Contains(second, "(today is 2026-10-23, the same search run 3 min ago)") || strings.Contains(first, "same search") {
		t.Errorf("cache note wrong:\nfirst: %s\nsecond: %s", first, second)
	}
	if strings.SplitN(first, "\n", 2)[1] != strings.SplitN(second, "\n", 2)[1] {
		t.Errorf("the cached results differ:\n%s\n%s", first, second)
	}

	search(`{"query":"go","recency":"week"}`)
	search(`{"query":"go","allowed_domains":["go.dev"]}`)
	search(`{"query":"go","limit":3}`)
	if requests.Load() != 4 {
		t.Errorf("%d requests after three differing searches, want 4", requests.Load())
	}

	*now = now.Add(fetchCacheTTL)
	search(`{"query":"go"}`)
	if requests.Load() != 5 {
		t.Errorf("an expired search was not run again: %d requests, want 5", requests.Load())
	}

	found.Store(false)
	search(`{"query":"nothing"}`)
	search(`{"query":"nothing"}`)
	if requests.Load() != 7 {
		t.Errorf("an empty answer was cached: %d requests, want 7", requests.Load())
	}
}

// TestSearchCacheKeepsTheNewest checks that the cache drops its oldest answers past
// maxCachedSearches.
func TestSearchCacheKeepsTheNewest(t *testing.T) {
	now := useSearchClock(t)
	var c searchCache
	for i := range maxCachedSearches + 5 {
		*now = now.Add(time.Second)
		c.put(searchQuery{text: string(rune('a' + i))}.key(5), searchAnswer{source: "x"})
	}
	if len(c.answers) != maxCachedSearches {
		t.Errorf("%d answers kept, want %d", len(c.answers), maxCachedSearches)
	}
	if _, ok := c.get(searchQuery{text: "a"}.key(5)); ok {
		t.Error("the oldest answer was kept")
	}
	if _, ok := c.get(searchQuery{text: string(rune('a' + maxCachedSearches + 4))}.key(5)); !ok {
		t.Error("the newest answer was dropped")
	}
}

// Storing an answer frees every answer that has expired.
func TestStoringASearchFreesExpiredAnswers(t *testing.T) {
	now := useSearchClock(t)
	var c searchCache
	c.put("old", searchAnswer{source: "x"})
	*now = now.Add(fetchCacheTTL)
	c.put("new", searchAnswer{source: "x"})
	if _, ok := c.answers["old"]; ok {
		t.Error("an expired answer is still held after another was stored")
	}
}
