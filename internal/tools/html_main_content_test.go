package tools

import "testing"

// htmlToText keeps a page's content and drops what surrounds it: only <main>, or the
// <article> elements, after the title; otherwise the page without its navigation, footer and
// sidebars.
func TestHTMLToTextKeepsTheMainContent(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			"main only, after the title",
			"<html><head><title>Fetch &amp; parse</title></head><body><header>Site</header><nav>Home Blog</nav>" +
				"<main><h1>API</h1><aside>Note: beta</aside><p>body</p></main><footer>(c) 2026</footer></body></html>",
			"Fetch & parse\n# API\n\nNote: beta\n\nbody",
		},
		{
			"articles when there is no main",
			"<nav>menu</nav><article><h2>One</h2><p>a</p></article><div>between</div><article><p>b</p></article><footer>f</footer>",
			"## One\n\na\n\nbetween\n\nb",
		},
		{
			"neither: page chrome dropped, header kept",
			"<header><h1>Title</h1></header><nav><a href='/'>Home</a></nav><div>text</div><aside>ads</aside><footer>links</footer>",
			"# Title\n\ntext",
		},
		{
			"markup quoted in a script does not count",
			"<head><script>var t = '<main>fake</main>';</script></head><body><nav>n</nav><p>real</p></body>",
			"real",
		},
		{
			"main in a comment does not count",
			"<!-- <main>old</main> --><nav>n</nav><p>real</p>",
			"real",
		},
		{
			"an empty main falls back to the page",
			"<nav>n</nav><main><div id='app'></div></main><p>server text</p>",
			"server text",
		},
		{
			"an unclosed main runs to the end",
			"<nav>n</nav><main><p>a</p><p>b",
			"a\n\nb",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToText(tc.in); got != tc.want {
				t.Errorf("htmlToText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
