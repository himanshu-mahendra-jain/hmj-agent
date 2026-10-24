package tools

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// maxSearchPageBytes caps a search engine's result page; Brave's runs to about 300KB.
const maxSearchPageBytes = 2 * 1024 * 1024

// fetchSearchPage GETs a search engine's result page as a browser would and returns its HTML.
func fetchSearchPage(ctx context.Context, client *http.Client, pageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchPageBytes))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// withQuery returns endpoint with params added to its query string.
func withQuery(endpoint string, params url.Values) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	maps.Copy(q, params)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// splitBlocks cuts body into the pieces that start at each match of start, each running to the
// next match or the end.
func splitBlocks(body string, start *regexp.Regexp) []string {
	idx := start.FindAllStringIndex(body, -1)
	blocks := make([]string, len(idx))
	for i, m := range idx {
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		blocks[i] = body[m[0]:end]
	}
	return blocks
}

// firstGroup returns re's first group in s, as plain text, or "".
func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return cleanHTML(m[1])
	}
	return ""
}

var (
	reBraveResult  = regexp.MustCompile(`<div[^>]*class="snippet[^"]*"[^>]*data-type="web"`)
	reBraveLink    = regexp.MustCompile(`<a[^>]*href="(https?://[^"]+)"`)
	reBraveTitle   = regexp.MustCompile(`(?s)<div[^>]*class="title[^"]*"[^>]*>(.*?)</div>`)
	reBraveContent = regexp.MustCompile(`(?s)<div[^>]*class="content[^"]*"[^>]*>(.*?)</div>`)
	reBraveAnswer  = regexp.MustCompile(`(?s)<div[^>]*class="inline-qa-answer[^"]*"[^>]*>(.*?)</div>`)
	reBraveAge     = regexp.MustCompile(`(?s)^\s*<span class="t-secondary">(.*?)</span>`)
	reBraveCaptcha = regexp.MustCompile(`(?i)captcha|are you a robot`)
	reHTMLComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// searchBrave reads Brave Search's result page.
func (t *WebSearchTool) searchBrave(ctx context.Context, client *http.Client, q searchQuery) ([]SearchResult, error) {
	params := url.Values{"q": {q.engineText()}, "source": {"web"}}
	if tf := braveRecency[q.recency]; tf != "" {
		params.Set("tf", tf)
	}
	pageURL, err := withQuery(t.braveEndpoint(), params)
	if err != nil {
		return nil, err
	}
	body, err := fetchSearchPage(ctx, client, pageURL)
	if err != nil {
		return nil, err
	}
	results := parseBraveResults(body, q.limit)
	if len(results) == 0 && reBraveCaptcha.MatchString(body) {
		return nil, errors.New("bot challenge")
	}
	return results, nil
}

// parseBraveResults reads the web results of a Brave Search page: its link, title, the date
// Brave puts before the description, and the description, or for a question site the top
// answer.
func parseBraveResults(body string, limit int) []SearchResult {
	var results resultList
	for _, block := range splitBlocks(reHTMLComment.ReplaceAllString(body, ""), reBraveResult) {
		if len(results.items) >= limit {
			break
		}
		r := SearchResult{URL: html.UnescapeString(firstRaw(reBraveLink, block)), Title: firstGroup(reBraveTitle, block)}
		content := firstRaw(reBraveContent, block)
		if m := reBraveAge.FindStringSubmatch(content); m != nil {
			r.Age = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cleanHTML(m[1])), "-"))
			content = content[len(m[0]):]
		}
		r.Snippet = cleanHTML(content)
		if r.Snippet == "" {
			if answer := firstGroup(reBraveAnswer, block); answer != "" {
				r.Snippet = "Top answer: " + answer
			}
		}
		results.add(r)
	}
	return results.items
}

// firstRaw returns re's first group in s as it is, or "".
func firstRaw(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

var (
	reBingResult  = regexp.MustCompile(`<li[^>]*class="b_algo`)
	reBingLink    = regexp.MustCompile(`(?s)<h2[^>]*>\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reBingSnippet = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	reBingAge     = regexp.MustCompile(`(?s)^\s*<span class="news_dt">(.*?)</span>(?:&nbsp;|\s)*(?:&#0183;|·)?(?:&#32;|\s)*`)
)

// searchBing reads Bing's result page.
func (t *WebSearchTool) searchBing(ctx context.Context, client *http.Client, q searchQuery) ([]SearchResult, error) {
	params := url.Values{"q": {q.engineText()}, "setlang": {"en-US"}}
	if f := bingRecency[q.recency]; f != "" {
		params.Set("filters", f)
	}
	pageURL, err := withQuery(t.bingEndpoint(), params)
	if err != nil {
		return nil, err
	}
	body, err := fetchSearchPage(ctx, client, pageURL)
	if err != nil {
		return nil, err
	}
	return parseBingResults(body, q.limit), nil
}

// parseBingResults reads the organic results of a Bing page, resolving Bing's click-tracking
// links to the pages they lead to.
func parseBingResults(body string, limit int) []SearchResult {
	var results resultList
	for _, block := range splitBlocks(body, reBingResult) {
		if len(results.items) >= limit {
			break
		}
		m := reBingLink.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		r := SearchResult{URL: decodeBingLink(html.UnescapeString(m[1])), Title: cleanHTML(m[2])}
		snippet := firstRaw(reBingSnippet, block[strings.Index(block, m[0])+len(m[0]):])
		if a := reBingAge.FindStringSubmatch(snippet); a != nil {
			r.Age = cleanHTML(a[1])
			snippet = snippet[len(a[0]):]
		}
		r.Snippet = cleanHTML(snippet)
		results.add(r)
	}
	return results.items
}

// decodeBingLink returns the page a Bing click-tracking link (bing.com/ck/a?...&u=a1<base64>)
// leads to, or the link itself when it is not one.
func decodeBingLink(link string) string {
	u, err := url.Parse(link)
	if err != nil || !strings.HasSuffix(u.Hostname(), "bing.com") {
		return link
	}
	enc, ok := strings.CutPrefix(u.Query().Get("u"), "a1")
	if !ok {
		return link
	}
	dec, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(enc, "="))
	if err != nil || !strings.HasPrefix(string(dec), "http") {
		return link
	}
	return string(dec)
}

// searchWikipedia runs a full-text search of English Wikipedia's articles.
func (t *WebSearchTool) searchWikipedia(ctx context.Context, client *http.Client, q searchQuery) ([]SearchResult, error) {
	reqURL, err := withQuery(t.wikipediaEndpoint(), url.Values{
		"action": {"query"}, "list": {"search"}, "srsearch": {q.text}, "srlimit": {fmt.Sprint(q.limit)},
		"srprop": {"snippet|timestamp"}, "format": {"json"}, "formatversion": {"2"},
	})
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
		Query *struct {
			Search []struct {
				Title     string `json:"title"`
				Snippet   string `json:"snippet"`
				Timestamp string `json:"timestamp"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 2*1024*1024), &data); err != nil || data.Query == nil {
		return nil, errors.New("unexpected Wikipedia response format")
	}
	var results resultList
	for _, s := range data.Query.Search {
		if len(results.items) >= q.limit {
			break
		}
		r := SearchResult{
			Title:   s.Title,
			URL:     wikipediaArticleURL + wikipediaTitlePath.Replace(url.PathEscape(strings.ReplaceAll(s.Title, " ", "_"))),
			Snippet: cleanHTML(s.Snippet),
		}
		if date, _, _ := strings.Cut(s.Timestamp, "T"); date != "" {
			r.Age = "updated " + date
		}
		results.add(r)
	}
	return results.items, nil
}

// wikipediaArticleURL is where the English Wikipedia articles live.
const wikipediaArticleURL = "https://en.wikipedia.org/wiki/"

// wikipediaTitlePath keeps the parentheses of a title such as "Go (programming language)" in
// its article path, as Wikipedia writes it.
var wikipediaTitlePath = strings.NewReplacer("%28", "(", "%29", ")")
