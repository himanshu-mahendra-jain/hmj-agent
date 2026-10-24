package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// ddgResult is one result as DuckDuckGo's HTML page marks it up, with snippet "" for a
// result that has none.
func ddgResult(class, href, title, snippet string) string {
	s := fmt.Sprintf(`<div class="result results_links results_links_deep %s">
  <div class="links_main links_deep result__body">
    <h2 class="result__title"><a rel="nofollow" class="result__a" href="%s">%s</a></h2>
    <div class="result__extras"><a class="result__url" href="%s">%s</a></div>`, class, href, title, href, href)
	if snippet != "" {
		s += fmt.Sprintf(`
    <a class="result__snippet" href="%s">%s</a>`, href, snippet)
	}
	return s + "\n  </div>\n</div>\n"
}

// TestParseDDGResultsKeepsSnippetsWithTheirResults checks a DuckDuckGo page with an ad, a
// result without a snippet, a repeated URL and a redirect link: each snippet stays with its
// own result, and the ad and the repeat are left out.
func TestParseDDGResultsKeepsSnippetsWithTheirResults(t *testing.T) {
	page := ddgResult("result--ad", "//duckduckgo.com/y.js?ad_domain=ads.example&amp;u3=x", "Buy now", "an ad") +
		ddgResult("web-result", "https://one.example/", "One &amp; only", "first <b>match</b>") +
		ddgResult("web-result", "https://two.example/", "Two", "") +
		ddgResult("web-result", "//duckduckgo.com/l/?uddg=https%3A%2F%2Fthree.example%2Fa%3Fb%3D1&amp;rut=x", "Three", "third") +
		ddgResult("web-result", "https://one.example", "One again", "repeat")
	got := parseDDGResults(page, 10)
	want := []SearchResult{
		{Title: "One & only", URL: "https://one.example/", Snippet: "first match"},
		{Title: "Two", URL: "https://two.example/"},
		{Title: "Three", URL: "https://three.example/a?b=1", Snippet: "third"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if got := parseDDGResults(page, 2); len(got) != 2 {
		t.Errorf("limit 2 gave %d results", len(got))
	}
}

// TestWebSearchInstantTopics checks the instant answer fallback: a topic's linked name is its
// title and the rest of its text its snippet, and grouped topics are included.
func TestWebSearchInstantTopics(t *testing.T) {
	tool := searchBackends{
		ddg: reply(`<html>nothing</html>`),
		instant: reply(`{"RelatedTopics":[
			{"Text":"Go (programming language) - A statically typed language.","FirstURL":"https://ddg.example/Go","Result":"<a href=\"https://ddg.example/Go\">Go (programming language)</a> - A statically typed language."},
			{"Name":"Games","Topics":[{"Text":"Go (game) An abstract strategy game.","FirstURL":"https://ddg.example/Go_game","Result":"<a href=\"x\">Go (game)</a> An abstract strategy game."}]}
		]}`),
	}.tool(t)
	out, err := tool.Execute(context.Background(), `{"query":"go"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1. [Go (programming language)](https://ddg.example/Go)\n   A statically typed language.",
		"2. [Go (game)](https://ddg.example/Go_game)\n   An abstract strategy game.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// braveResult is one web result as Brave Search marks it up, with age "" for one without a
// date.
func braveResult(href, title, age, text string) string {
	content := text
	if age != "" {
		content = `<!--[0--><span class="t-secondary">` + age + `</span><!--]--> <!---->` + text
	}
	return fmt.Sprintf(`<div class="snippet svelte-jmfu5f" data-pos="0" data-type="web" data-keynav="true"><div class="result-body"><div class="result-content"><a href="%s" target="_self" class="svelte-14r20fy l1"><div class="site-name-wrapper"><cite class="snippet-url">site</cite></div> <div class="title search-snippet-title line-clamp-1 svelte-14r20fy" title="%s">%s</div></a><!----> <!--[0--><div class="generic-snippet svelte-1cwdgg3"><div class="content desktop-default-regular t-primary line-clamp-dynamic svelte-1cwdgg3">%s<!----></div></div></div></div></div>`, href, title, title, content)
}

// TestParseBraveResults checks Brave's results: the date before a description becomes the age,
// a question site's top answer stands in for a missing description, and other kinds of
// snippet are left out.
func TestParseBraveResults(t *testing.T) {
	qa := `<div class="snippet svelte-jmfu5f" data-pos="1" data-type="web"><a href="https://qa.example/q/1" class="l1"><div class="title search-snippet-title" title="A question">A question</div></a> <div class="inline-qa"><div class="inline-qa-answer desktop-default-regular top-answer"><p><strong>Update</strong>: use WithoutCancel.</p></div></div></div>`
	page := `<div class="snippet svelte-x" data-type="video"><a href="https://video.example/">a video</a></div>` +
		braveResult("https://one.example/a?x=1&amp;y=2", "One &amp; more", "July 15, 2020 -", "first <strong>match</strong>") +
		qa +
		braveResult("https://two.example/", "Two", "", "second")
	got := parseBraveResults(page, 10)
	want := []SearchResult{
		{Title: "One & more", URL: "https://one.example/a?x=1&y=2", Snippet: "first match", Age: "July 15, 2020"},
		{Title: "A question", URL: "https://qa.example/q/1", Snippet: "Top answer: Update: use WithoutCancel."},
		{Title: "Two", URL: "https://two.example/", Snippet: "second"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// bingResult is one organic result as Bing marks it up, its link through Bing's click tracking.
func bingResult(target, title, age, text string) string {
	link := "https://www.bing.com/ck/a?!&amp;&amp;p=abc&amp;ptn=3&amp;u=a1" + base64.RawURLEncoding.EncodeToString([]byte(target)) + "&amp;ntb=1"
	if age != "" {
		text = `<span class="news_dt">` + age + `</span>&nbsp;&#0183;&#32;` + text
	}
	return fmt.Sprintf(`<li class="b_algo" data-id iid=SERP.1><div class="b_tpcn"><a class="tilk" href="%s"><div class="tptt">site</div><cite>%s</cite></a></div><h2><a target="_blank" href="%s" h="ID=SERP,1">%s</a></h2><div class="b_caption"><p class="b_lineclamp2">%s</p></div></li>`, link, target, link, title, text)
}

// TestParseBingResults checks Bing's results: the click-tracking link resolves to the page,
// and a date before the snippet becomes the age.
func TestParseBingResults(t *testing.T) {
	page := `<ol id="b_results">` + bingResult("https://one.example/doc", "One <strong>doc</strong>", "Sep 3, 2025", "first") +
		bingResult("https://two.example/", "Two", "", "second &amp; last") + `<li class="b_ans">an answer box</li></ol>`
	got := parseBingResults(page, 10)
	want := []SearchResult{
		{Title: "One doc", URL: "https://one.example/doc", Snippet: "first", Age: "Sep 3, 2025"},
		{Title: "Two", URL: "https://two.example/", Snippet: "second & last"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// TestDecodeBingLink checks that only Bing's own tracking links are rewritten.
func TestDecodeBingLink(t *testing.T) {
	for link, want := range map[string]string{
		"https://www.bing.com/ck/a?u=a1aHR0cHM6Ly9nby5kZXYv&ntb=1": "https://go.dev/",
		"https://www.bing.com/ck/a?u=zzz":                          "https://www.bing.com/ck/a?u=zzz",
		"https://www.bing.com/ck/a?u=a1!!!":                        "https://www.bing.com/ck/a?u=a1!!!",
		"https://other.example/?u=a1aHR0cHM6Ly9nby5kZXYv":          "https://other.example/?u=a1aHR0cHM6Ly9nby5kZXYv",
	} {
		if got := decodeBingLink(link); got != want {
			t.Errorf("decodeBingLink(%q) = %q, want %q", link, got, want)
		}
	}
}

// TestClipSnippet checks that a long snippet is cut on a character boundary and its lines
// joined.
func TestClipSnippet(t *testing.T) {
	got := clipSnippet("line one\n\n  line two " + strings.Repeat("é", 300))
	if !strings.HasPrefix(got, "line one line two é") || !strings.HasSuffix(got, "…") || len(got) > maxSnippetBytes+len("…") || !utf8.ValidString(got) {
		t.Errorf("clipSnippet = %d bytes: %q", len(got), got)
	}
}
