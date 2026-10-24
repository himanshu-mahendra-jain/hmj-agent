package tools

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// fetchCacheTTL is how long fetch_url answers a URL from its last fetch.
	fetchCacheTTL = 15 * time.Minute
	// maxCachedPages and maxCachedBytes bound the cache; the oldest pages go first. A page
	// over maxCachedBytes is not kept.
	maxCachedPages = 64
	maxCachedBytes = 16 * 1024 * 1024
)

// cachedPage is the text fetch_url made of a response, with the notes on how it was made.
type cachedPage struct {
	text      string
	notes     []string
	fetchedAt time.Time
}

var (
	pageCacheMu sync.Mutex
	pageCache   = map[string]cachedPage{}
	// cacheNow is the cache's clock, replaced by tests.
	cacheNow = time.Now
)

// cachedFetch returns the page last fetched from rawURL when it is under fetchCacheTTL old.
func cachedFetch(rawURL string) (cachedPage, bool) {
	pageCacheMu.Lock()
	defer pageCacheMu.Unlock()
	page, ok := pageCache[rawURL]
	if ok && cacheNow().Sub(page.fetchedAt) >= fetchCacheTTL {
		delete(pageCache, rawURL)
		return cachedPage{}, false
	}
	return page, ok
}

// storeFetch keeps the text fetched from u for later calls, unless u is on this machine or the
// local network, whose pages change as you work on them.
func storeFetch(u *url.URL, text string, notes []string) {
	if isLocalHost(u.Hostname()) {
		return
	}
	pageCacheMu.Lock()
	defer pageCacheMu.Unlock()
	now := cacheNow()
	pageCache[u.String()] = cachedPage{text: text, notes: slices.Clone(notes), fetchedAt: now}

	keys := make([]string, 0, len(pageCache))
	total := 0
	for k, p := range pageCache {
		if now.Sub(p.fetchedAt) >= fetchCacheTTL {
			delete(pageCache, k)
			continue
		}
		keys = append(keys, k)
		total += len(p.text)
	}
	slices.SortFunc(keys, func(a, b string) int { return pageCache[a].fetchedAt.Compare(pageCache[b].fetchedAt) })
	for _, k := range keys {
		if len(pageCache) <= maxCachedPages && total <= maxCachedBytes {
			break
		}
		total -= len(pageCache[k].text)
		delete(pageCache, k)
	}
}

// ageNote says how old a cached page is.
func (p cachedPage) ageNote() string {
	if age := cacheNow().Sub(p.fetchedAt); age >= time.Minute {
		return fmt.Sprintf("a copy fetched %d min ago", int(age/time.Minute))
	}
	return "a copy fetched less than a minute ago"
}

// isLocalHost reports whether host names this machine or the local network: localhost, a
// loopback, private or link-local address, or a name without a dot or under .local, .internal
// or .localhost.
func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
