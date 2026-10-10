package tools

import (
	"strings"
	"testing"
	"time"
)

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"headings by level", "<h1>Top</h1><h3>Sub</h3>text", "# Top\n\n### Sub\n\ntext"},
		{"paragraphs", "<p>one</p><p>two</p>", "one\n\ntwo"},
		{"list items", "<ul><li>a</li><li>b<li>c</ul>", "* a\n* b\n* c"},
		{"link", `see <a class="x" href="https://e.com/?a=1&amp;b=2">the <b>docs</b></a>.`, "see [the docs](https://e.com/?a=1&b=2)."},
		{"single-quoted and bare hrefs", `<a href='/x'>x</a> <A HREF=/y>y</A>`, "[x](/x) [y](/y)"},
		{"javascript link dropped", `<a href="javascript:void(0)">menu</a>ok`, "ok"},
		{"anchor without href keeps text", `<a name="top">Top</a>`, "Top"},
		{"hidden elements", "<script>var a = '</p>';</script><style>p{}</style><noscript>nojs</noscript><svg><path d='M0'/></svg>shown", "shown"},
		{"script end tag case and spacing", "<SCRIPT>x</Script >after", "after"},
		{"unclosed script hides the rest", "before<script>secret", "before"},
		{"self-closing svg hides nothing", "<svg/>after", "after"},
		{"comment", "a<!-- <p>hidden</p> -->b", "ab"},
		{"pre block", "<pre><code>x := 1\n  y</code></pre>", "```\nx := 1\ny\n```"},
		{"pre is not a paragraph", "<pre>code</pre>", "```\ncode\n```"},
		{"link tag is not a list item", `<link rel="x" href="y">text`, "text"},
		{"quoted > inside attribute", `<img alt="a > b">text`, "text"},
		{"apostrophe outside a value", `<p it's>text</p>`, "text"},
		{"literal less-than", "a < b && c > d", "a < b && c > d"},
		{"entities", "&lt;b&gt; &amp; &#x41; &copy;", "<b> & A ©"},
		{"br and hr", "a<br>b<br/>c<hr>d", "a\nb\nc\n---\nd"},
		{"table cells", "<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>", "a b\nc"},
		{"divs separate text", "<div>a</div><div>b</div>", "a\nb"},
		{"unterminated tag", "text<div class=", "text"},
		{"doctype and title", "<!DOCTYPE html><html><head><title>T</title></head><body>b</body></html>", "T\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToText(tc.in); got != tc.want {
				t.Errorf("htmlToText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// A large page converts in one pass; the old regex pipeline took ~0.5s for 4MB.
func TestHTMLToTextLargePageIsFast(t *testing.T) {
	block := `<div class="row"><h2 id="a">Heading</h2><p>Some <b>bold</b> text with <a href="https://example.com/x">a link</a> &amp; more.</p>` +
		`<ul><li>one</li><li>two</li></ul><!-- c --><script>console.log("x")</script></div>` + "\n"
	page := strings.Repeat(block, MaxFetchBodyBytes/len(block))
	start := time.Now()
	out := htmlToText(page)
	if elapsed := time.Since(start); elapsed > 2*time.Second && !raceEnabled {
		t.Errorf("converting %d bytes took %v", len(page), elapsed)
	}
	if !strings.Contains(out, "[a link](https://example.com/x)") || strings.Contains(out, "console.log") {
		t.Errorf("unexpected output start: %q", out[:200])
	}
}
