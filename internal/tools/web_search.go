package tools

import (
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
	// DDGHTMLEndpoint, DDGInstantEndpoint, and WikipediaEndpoint allow tests
	// to point the backends at a local mock server. Zero values use the
	// production endpoints.
	DDGHTMLEndpoint    string
	DDGInstantEndpoint string
	WikipediaEndpoint  string
}

func (t *WebSearchTool) ddgHTMLEndpoint() string {
	if t.DDGHTMLEndpoint != "" {
		return t.DDGHTMLEndpoint
	}
	return "https://html.duckduckgo.com/html/"
}

func (t *WebSearchTool) ddgInstantEndpoint() string {
	if t.DDGInstantEndpoint != "" {
		return t.DDGInstantEndpoint
	}
	return "https://api.duckduckgo.com/?q=%s&format=json&no_html=1&skip_disambig=0"
}

func (t *WebSearchTool) wikipediaEndpoint() string {
	if t.WikipediaEndpoint != "" {
		return t.WikipediaEndpoint
	}
	return "https://en.wikipedia.org/w/api.php?action=opensearch&search=%s&limit=%d&namespace=0&format=json"
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name: "web_search",
			Description: "Search the web for documentation, tutorials, code snippets, package APIs, or error messages. " +
				"Returns top search result titles, URLs, and summaries.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query string.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Optional number of results to return (default 5, max 10).",
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
	}
}

type webSearchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

var (
	reDDGLink    = regexp.MustCompile(`(?is)<a[^>]*class=['"][^'"]*(?:result__a|result-link)[^'"]*['"][^>]*href=['"]([^'"]+)['"][^>]*>(.*?)</a>`)
	reAllTags    = regexp.MustCompile(`<[^>]+>`)
	reDDGSnippet = regexp.MustCompile(`(?is)<(?:a|td|div)[^>]*class=['"][^'"]*(?:result__snippet|result-snippet)[^'"]*['"][^>]*>(.*?)</(?:a|td|div)>`)
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

	client := t.Client
	if client == nil {
		client = publicSearchClient()
	}

	var backendErrs []string

	// 1. Try DuckDuckGo HTML / Lite search
	results, err := t.searchDDG(ctx, client, query, limit)
	if err == nil && len(results) > 0 {
		return formatSearchResults(query, results), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		backendErrs = append(backendErrs, fmt.Sprintf("duckduckgo: %v", err))
	}

	// 2. Fallback: DuckDuckGo Instant Answer API
	results, err = t.searchDDGInstant(ctx, client, query, limit)
	if err == nil && len(results) > 0 {
		return formatSearchResults(query, results), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		backendErrs = append(backendErrs, fmt.Sprintf("duckduckgo_instant: %v", err))
	}

	// 3. Fallback: Wikipedia OpenSearch API
	results, err = t.searchWikipedia(ctx, client, query, limit)
	if err == nil && len(results) > 0 {
		return formatSearchResults(query, results), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		backendErrs = append(backendErrs, fmt.Sprintf("wikipedia: %v", err))
	}

	if len(backendErrs) == 3 {
		return fmt.Sprintf("No search results found for %q (all backends failed: %s). Try refining the search query or use fetch_url directly with a known documentation URL.", query, strings.Join(backendErrs, "; ")), nil
	}

	return fmt.Sprintf("No search results found for %q. Try refining the search query or use fetch_url directly with a known documentation URL.", query), nil
}

func (t *WebSearchTool) searchDDG(ctx context.Context, client *http.Client, query string, limit int) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", query)

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

	links := reDDGLink.FindAllStringSubmatch(body, -1)
	snippets := reDDGSnippet.FindAllStringSubmatch(body, -1)

	var results []SearchResult
	for i, link := range links {
		if len(results) >= limit {
			break
		}
		rawHref := link[1]
		cleanURL := decodeDDGRedirect(rawHref)
		rawTitle := link[2]
		cleanTitle := cleanHTML(rawTitle)

		snippet := ""
		if i < len(snippets) {
			snippet = cleanHTML(snippets[i][1])
		}

		if cleanTitle != "" && cleanURL != "" {
			results = append(results, SearchResult{
				Title:   cleanTitle,
				URL:     cleanURL,
				Snippet: snippet,
			})
		}
	}

	return results, nil
}

func (t *WebSearchTool) searchDDGInstant(ctx context.Context, client *http.Client, query string, limit int) ([]SearchResult, error) {
	reqURL := fmt.Sprintf(t.ddgInstantEndpoint(), url.QueryEscape(query))
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
		Heading       string `json:"Heading"`
		AbstractText  string `json:"AbstractText"`
		AbstractURL   string `json:"AbstractURL"`
		RelatedTopics []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
		} `json:"RelatedTopics"`
	}

	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 2*1024*1024), &data); err != nil {
		return nil, err
	}

	var results []SearchResult
	if data.AbstractText != "" && data.AbstractURL != "" {
		title := data.Heading
		if title == "" {
			title = query
		}
		results = append(results, SearchResult{
			Title:   title,
			URL:     data.AbstractURL,
			Snippet: data.AbstractText,
		})
	}

	for _, rt := range data.RelatedTopics {
		if len(results) >= limit {
			break
		}
		if rt.Text != "" && rt.FirstURL != "" {
			results = append(results, SearchResult{
				Title:   rt.Text,
				URL:     rt.FirstURL,
				Snippet: rt.Text,
			})
		}
	}

	return results, nil
}

func (t *WebSearchTool) searchWikipedia(ctx context.Context, client *http.Client, query string, limit int) ([]SearchResult, error) {
	reqURL := fmt.Sprintf(t.wikipediaEndpoint(), url.QueryEscape(query), limit)
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

	// Wikipedia opensearch returns: [query, [titles...], [descriptions...], [urls...]]
	var data []any
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 2*1024*1024), &data); err != nil || len(data) < 4 {
		return nil, errors.New("unexpected Wikipedia response format")
	}

	titles, ok1 := data[1].([]any)
	descriptions, ok2 := data[2].([]any)
	urls, ok3 := data[3].([]any)
	if !ok1 || !ok2 || !ok3 {
		return nil, errors.New("failed parsing Wikipedia array")
	}

	var results []SearchResult
	for i, item := range titles {
		if len(results) >= limit {
			break
		}
		if i >= len(urls) {
			// URLs are strictly required; descriptions are optional and the
			// array can be shorter or omitted, so only bail when the link
			// itself is missing (title-only rows are unusable).
			break
		}
		title, okTitle := item.(string)
		if !okTitle {
			continue // not a usable result row
		}
		desc := ""
		if i < len(descriptions) {
			if d, okDesc := descriptions[i].(string); okDesc {
				desc = d
			}
		}
		link, okLink := urls[i].(string)
		if !okLink {
			continue // not a usable result row
		}
		if title != "" && link != "" {
			results = append(results, SearchResult{
				Title:   title,
				URL:     link,
				Snippet: desc,
			})
		}
	}

	return results, nil
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

func formatSearchResults(query string, results []SearchResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for %q:\n\n", query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. [%s](%s)\n", i+1, r.Title, r.URL))
		if r.Snippet != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", r.Snippet))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("To read the full text of any page, use fetch_url with the URL.")
	return strings.TrimSpace(sb.String())
}
