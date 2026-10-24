package tools

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxCachedSearches bounds a WebSearchTool's cache; the oldest searches go first.
const maxCachedSearches = 100

// searchAnswer is what one search found: the results, the source that gave them, and the
// sources that failed before it.
type searchAnswer struct {
	results  []SearchResult
	source   string
	failures []string
	at       time.Time
}

// searchCache keeps a WebSearchTool's answers for fetchCacheTTL, so a search the model repeats
// within a turn or two costs no request. Failures and empty answers are not kept, so a retry
// tries again.
type searchCache struct {
	mu      sync.Mutex
	answers map[string]searchAnswer
}

// key identifies a search by everything that shapes its answer.
func (q searchQuery) key(limit int) string {
	return strings.Join([]string{q.text, strings.Join(q.allowed, ","), strings.Join(q.blocked, ","), q.recency, strconv.Itoa(limit)}, "\x00")
}

// ageLabel says when a cached answer was found.
func (a searchAnswer) ageLabel() string {
	if age := cacheNow().Sub(a.at); age >= time.Minute {
		return fmt.Sprintf("run %d min ago", int(age/time.Minute))
	}
	return "run less than a minute ago"
}

func (c *searchCache) get(key string) (searchAnswer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.answers[key]
	if ok && cacheNow().Sub(a.at) >= fetchCacheTTL {
		delete(c.answers, key)
		return searchAnswer{}, false
	}
	return a, ok
}

func (c *searchCache) put(key string, a searchAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := cacheNow()
	a.at = now
	if c.answers == nil {
		c.answers = map[string]searchAnswer{}
	}
	c.answers[key] = a
	keys := make([]string, 0, len(c.answers))
	for k, v := range c.answers {
		if now.Sub(v.at) >= fetchCacheTTL {
			delete(c.answers, k)
			continue
		}
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y string) int { return c.answers[x].at.Compare(c.answers[y].at) })
	for _, k := range keys[:max(len(keys)-maxCachedSearches, 0)] {
		delete(c.answers, k)
	}
}
