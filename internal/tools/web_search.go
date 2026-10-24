package tools

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"hmj-agent/internal/chat"
)

// searchClient and searchClientOnce back the package-level pooled client for
// outbound search traffic. This is deliberately separate from the fetch_url
// client (different timeout and redirect policy) and the chat client (no SSE
// streaming needs) so each policy stays independent.
var (
	searchClient     *http.Client
	searchClientOnce sync.Once
)

// publicSearchClient returns the shared pooled client with a hard 15s timeout.
func publicSearchClient() *http.Client {
	searchClientOnce.Do(func() {
		searchClient = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				// Unlike fetch_url which accepts arbitrary user-supplied URLs, web_search only
				// queries hardcoded public endpoints (DuckDuckGo, Wikipedia) with query-escaped
				// parameters. We intentionally retain ProxyFromEnvironment so developers behind
				// enterprise/corporate HTTP proxies can search without encountering network failures.
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
				ForceAttemptHTTP2:     true,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			},
			CheckRedirect: checkPublicRedirect,
		}
	})
	return searchClient
}

// checkPublicRedirect lets the search endpoints redirect only to public addresses, so a
// compromised or spoofed response cannot point a search at this machine or the private
// network. (fetch_url, which the model aims itself, may reach local services.)
func checkPublicRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to non-HTTP scheme %q blocked", req.URL.Scheme)
	}
	host := req.URL.Hostname()
	addrs, err := net.DefaultResolver.LookupNetIP(req.Context(), "ip", host)
	if err != nil {
		return fmt.Errorf("redirect to %s: %w", host, err)
	}
	for _, addr := range addrs {
		if addr = addr.Unmap(); !addr.IsGlobalUnicast() || addr.IsPrivate() {
			return fmt.Errorf("redirect to non-public address %s (%s) blocked", host, addr)
		}
	}
	return nil
}

type WebSearchTool struct {
	Client *http.Client
	// The endpoints let tests point the backends at a local mock server. Zero values use the
	// production endpoints.
	DDGHTMLEndpoint    string
	BraveEndpoint      string
	BingEndpoint       string
	DDGInstantEndpoint string
	WikipediaEndpoint  string

	cache searchCache
}

func (t *WebSearchTool) braveEndpoint() string {
	return cmp.Or(t.BraveEndpoint, "https://search.brave.com/search")
}

func (t *WebSearchTool) bingEndpoint() string {
	return cmp.Or(t.BingEndpoint, "https://www.bing.com/search")
}

func (t *WebSearchTool) ddgHTMLEndpoint() string {
	if t.DDGHTMLEndpoint != "" {
		return t.DDGHTMLEndpoint
	}
	return "https://html.duckduckgo.com/html/"
}

func (t *WebSearchTool) ddgInstantEndpoint() string {
	return cmp.Or(t.DDGInstantEndpoint, "https://api.duckduckgo.com/")
}

func (t *WebSearchTool) wikipediaEndpoint() string {
	return cmp.Or(t.WikipediaEndpoint, "https://en.wikipedia.org/w/api.php")
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name: "web_search",
			Description: "Search the web for documentation, tutorials, code snippets, package APIs, or error messages. " +
				"Returns top search result titles, URLs, dates when known, and summaries. " +
				"Narrow it with allowed_domains (e.g. the project's docs site), blocked_domains, and recency for news, releases and recent changes.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"query", "limit", "allowed_domains", "blocked_domains", "recency"},
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query string.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Optional number of results to return (default 5, max 10).",
					},
					"allowed_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Only return results from these domains and their subdomains, e.g. [\"go.dev\", \"pkg.go.dev\"].",
					},
					"blocked_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Never return results from these domains and their subdomains.",
					},
					"recency": map[string]any{
						"type":        "string",
						"enum":        searchRecencies,
						"description": "Only return pages from the past day, week, month or year.",
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
	}
}

type webSearchInput struct {
	Query          string   `json:"query"`
	Limit          int      `json:"limit"`
	AllowedDomains []string `json:"allowed_domains"`
	BlockedDomains []string `json:"blocked_domains"`
	Recency        string   `json:"recency"`
}

type SearchResult struct {
	Title   string
	URL     string
	Snippet string
	// Age is the date the source shows for the page, such as "Sep 3, 2025", when it shows one.
	Age string
}

// searchQuery is one search as the backends run it.
type searchQuery struct {
	text  string
	limit int // how many results a backend returns: more than the call shows, to filter
	// allowed and blocked are domains, as normalizeDomains gives them; recency is "" or one of
	// searchRecencies.
	allowed, blocked []string
	recency          string
}

// searchFetchLimit is how many results each backend is asked for. The call's limit is applied
// once repeats and filtered domains are gone.
const searchFetchLimit = 10

// searchBackend is one source of search results, tried in turn until one returns some.
type searchBackend struct {
	name   string // in error notes
	label  string // in the results' header
	search func(ctx context.Context, client *http.Client, q searchQuery) ([]SearchResult, error)
	// recency maps each recency the backend can filter by to its own name for it; a search
	// with a recency skips a backend without that one.
	recency map[string]string
}

// backends lists the sources in the order they are tried: DuckDuckGo's results page, then
// Brave's and Bing's, which answer when DuckDuckGo blocks the address, then DuckDuckGo's
// instant answers and a Wikipedia article search.
func (t *WebSearchTool) backends() []searchBackend {
	return []searchBackend{
		{"duckduckgo", "DuckDuckGo", t.searchDDG, ddgRecency},
		{"brave", "Brave", t.searchBrave, braveRecency},
		{"bing", "Bing", t.searchBing, bingRecency},
		{"duckduckgo_instant", "DuckDuckGo instant answers", t.searchDDGInstant, nil},
		{"wikipedia", "Wikipedia", t.searchWikipedia, nil},
	}
}

var (
	reDDGLink    = regexp.MustCompile(`(?is)<a[^>]*class=['"][^'"]*(?:result__a|result-link)[^'"]*['"][^>]*href=['"]([^'"]+)['"][^>]*>(.*?)</a>`)
	reAllTags    = regexp.MustCompile(`<[^>]+>`)
	reDDGSnippet = regexp.MustCompile(`(?is)<(?:a|td|div)[^>]*class=['"][^'"]*(?:result__snippet|result-snippet)[^'"]*['"][^>]*>(.*?)</(?:a|td|div)>`)
	// reInstantName finds the linked name at the start of an instant answer topic's HTML.
	reInstantName = regexp.MustCompile(`(?is)<a[^>]*>(.*?)</a>`)
)

func (t *WebSearchTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var in webSearchInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", errors.New("query is required")
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	q := searchQuery{text: query, limit: searchFetchLimit, recency: in.Recency}
	if q.recency != "" && !slices.Contains(searchRecencies, q.recency) {
		return "", fmt.Errorf("invalid recency %q: use day, week, month or year", in.Recency)
	}
	var err error
	if q.allowed, err = normalizeDomains("allowed_domains", in.AllowedDomains); err != nil {
		return "", err
	}
	if q.blocked, err = normalizeDomains("blocked_domains", in.BlockedDomains); err != nil {
		return "", err
	}

	key := q.key(limit)
	if a, ok := t.cache.get(key); ok {
		return formatSearchResults(query, a, true), nil
	}

	client := t.Client
	if client == nil {
		client = publicSearchClient()
	}

	var backendErrs []string
	tried := 0
	for _, b := range t.backends() {
		if q.recency != "" && b.recency[q.recency] == "" {
			continue
		}
		tried++
		results, err := b.search(ctx, client, q)
		results = slices.DeleteFunc(results, func(r SearchResult) bool { return !q.keeps(r) })
		if err == nil && len(results) > 0 {
			a := searchAnswer{results: results[:min(len(results), limit)], source: b.label, failures: backendErrs}
			t.cache.put(key, a)
			return formatSearchResults(query, a, false), nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			backendErrs = append(backendErrs, fmt.Sprintf("%s: %v", b.name, err))
		}
	}

	if len(backendErrs) == tried {
		return fmt.Sprintf("No search results found for %q (all backends failed: %s). Try refining the search query or use fetch_url directly with a known documentation URL.", query, strings.Join(backendErrs, "; ")), nil
	}

	return fmt.Sprintf("No search results found for %q. Try refining the search query or use fetch_url directly with a known documentation URL.", query), nil
}

func (t *WebSearchTool) searchDDG(ctx context.Context, client *http.Client, q searchQuery) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", q.engineText())
	if df := ddgRecency[q.recency]; df != "" {
		form.Set("df", df)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.ddgHTMLEndpoint(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted { // what DuckDuckGo answers an address it suspects
		return nil, errors.New("DDG bot challenge encountered (status 202)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DDG returned status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}

	body := string(bodyBytes)
	if strings.Contains(body, "anomaly-modal") {
		return nil, errors.New("DDG bot challenge encountered")
	}

	return parseDDGResults(body, q.limit), nil
}

// parseDDGResults reads the results of a DuckDuckGo HTML page. Each result runs from its title
// link to the next one, so a snippet is only ever paired with its own result, and a result
// without one gets none. Ads, which link through DuckDuckGo's y.js, and repeated URLs are left
// out.
func parseDDGResults(body string, limit int) []SearchResult {
	links := reDDGLink.FindAllStringSubmatchIndex(body, -1)
	var results resultList
	for i, link := range links {
		if len(results.items) >= limit {
			break
		}
		end := len(body)
		if i+1 < len(links) {
			end = links[i+1][0]
		}
		snippet := ""
		if m := reDDGSnippet.FindStringSubmatch(body[link[1]:end]); m != nil {
			snippet = cleanHTML(m[1])
		}
		href := decodeDDGRedirect(html.UnescapeString(body[link[2]:link[3]]))
		if isDDGAd(href) {
			continue
		}
		results.add(SearchResult{Title: cleanHTML(body[link[4]:link[5]]), URL: href, Snippet: snippet})
	}
	return results.items
}

// isDDGAd reports whether a result link is an ad, which DuckDuckGo routes through its y.js.
func isDDGAd(href string) bool {
	u, err := url.Parse(href)
	return err == nil && strings.HasSuffix(u.Hostname(), "duckduckgo.com") && u.Path == "/y.js"
}

// resultList collects search results, leaving out ones without a title or URL and repeats of a
// URL already listed.
type resultList struct {
	items []SearchResult
	seen  map[string]bool
}

func (l *resultList) add(r SearchResult) {
	if r.Title == "" || r.URL == "" {
		return
	}
	key := strings.TrimSuffix(strings.SplitN(r.URL, "#", 2)[0], "/")
	if l.seen[key] {
		return
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[key] = true
	l.items = append(l.items, r)
}

func (t *WebSearchTool) searchDDGInstant(ctx context.Context, client *http.Client, q searchQuery) ([]SearchResult, error) {
	query, limit := q.text, q.limit
	reqURL, err := withQuery(t.ddgInstantEndpoint(), url.Values{"q": {query}, "format": {"json"}, "no_html": {"1"}, "skip_disambig": {"0"}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", apiUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Heading       string         `json:"Heading"`
		AbstractText  string         `json:"AbstractText"`
		AbstractURL   string         `json:"AbstractURL"`
		RelatedTopics []instantTopic `json:"RelatedTopics"`
	}

	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 2*1024*1024), &data); err != nil {
		return nil, err
	}

	var results resultList
	if data.AbstractText != "" {
		results.add(SearchResult{Title: cmp.Or(data.Heading, query), URL: data.AbstractURL, Snippet: data.AbstractText})
	}
	var addTopics func([]instantTopic)
	addTopics = func(topics []instantTopic) {
		for _, rt := range topics {
			if len(results.items) >= limit {
				return
			}
			if len(rt.Topics) > 0 { // a group of topics under a heading
				addTopics(rt.Topics)
				continue
			}
			results.add(rt.result())
		}
	}
	addTopics(data.RelatedTopics)
	return results.items, nil
}

// instantTopic is one related topic of a DuckDuckGo instant answer, or a group of them.
type instantTopic struct {
	Text     string         `json:"Text"`
	FirstURL string         `json:"FirstURL"`
	Result   string         `json:"Result"` // HTML: the topic's name as a link, then its text
	Topics   []instantTopic `json:"Topics"`
}

// result splits the topic's text into its name, which Result links, and the rest.
func (rt instantTopic) result() SearchResult {
	title := rt.Text
	if m := reInstantName.FindStringSubmatch(rt.Result); m != nil {
		if name := cleanHTML(m[1]); name != "" && strings.HasPrefix(rt.Text, name) {
			title = name
		}
	}
	snippet := strings.TrimLeft(strings.TrimPrefix(rt.Text, title), " -–—,:")
	return SearchResult{Title: title, URL: rt.FirstURL, Snippet: snippet}
}

func decodeDDGRedirect(rawHref string) string {
	if strings.HasPrefix(rawHref, "//") {
		rawHref = "https:" + rawHref
	}
	u, err := url.Parse(rawHref)
	if err == nil && u.Query().Get("uddg") != "" {
		decoded, err := url.QueryUnescape(u.Query().Get("uddg"))
		if err == nil && decoded != "" {
			return decoded
		}
	}
	return rawHref
}

func cleanHTML(s string) string {
	stripped := reAllTags.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(stripped))
}

// formatSearchResults lists an answer's results under a header naming the source and today's
// date, against which the pages' dates, such as "2 days ago", can be read.
func formatSearchResults(query string, a searchAnswer, cached bool) string {
	var sb strings.Builder
	note := ""
	if cached {
		note = ", the same search " + a.ageLabel()
	}
	sb.WriteString(fmt.Sprintf("Search results for %q from %s (today is %s%s):\n\n", query, a.source, cacheNow().Format("2006-01-02"), note))
	for i, r := range a.results {
		sb.WriteString(fmt.Sprintf("%d. [%s](%s)\n", i+1, r.Title, r.URL))
		if text := strings.Join(slices.DeleteFunc([]string{r.Age, clipSnippet(r.Snippet)}, func(s string) bool { return s == "" }), " · "); text != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", text))
		}
		sb.WriteString("\n")
	}
	if len(a.failures) > 0 {
		sb.WriteString("Sources that failed first: " + strings.Join(a.failures, "; ") + "\n")
	}
	sb.WriteString("To read the full text of any page, use fetch_url with the URL.")
	return strings.TrimSpace(sb.String())
}

// maxSnippetBytes caps one result's snippet; a question site's top answer can run long.
const maxSnippetBytes = 400

// clipSnippet joins a snippet's lines and cuts it at maxSnippetBytes, on a character boundary.
func clipSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxSnippetBytes {
		return s
	}
	return strings.TrimSpace(TruncateToBytes(s, maxSnippetBytes)) + "…"
}
