package tools

import (
	"strings"
	"testing"
)

// FuzzHTMLToText checks that converting arbitrary markup never leaks script content,
// never grows without bound and always returns tidy text.
func FuzzHTMLToText(f *testing.F) {
	f.Add("<html><body><h1>Title</h1><p>Hello <b>world</b></p></body></html>")
	f.Add("<script>alert(1)</script><p>visible</p>")
	f.Add("<style>p{color:red}</style><noscript>no js</noscript>text")
	f.Add("<a href=\"http://x\">link</a> <a href=\"javascript:void(0)\">bad</a>")
	f.Add("<pre><code>a < b && c > d</code></pre>")
	f.Add("<ul><li>one<li>two</ul><hr><br>")
	f.Add("&lt;script&gt; &amp; &#x41; &#0; &unknown;")
	f.Add("<!-- comment --><svg><path d=\"M0 0\"/></svg>after")
	f.Add("<<<>>><p<p>><")
	f.Add("\x00\xff<p>\xc3(</p>")

	const secret = "SECRETSCRIPTBODY"
	f.Fuzz(func(t *testing.T, in string) {
		if strings.Contains(in, secret) {
			return
		}
		out := htmlToText("<script>" + secret + "</script>" + in)
		if strings.Contains(out, secret) {
			t.Fatalf("script content leaked into output %q for input %q", out, in)
		}
		out = htmlToText(in)
		if len(out) > 2*len(in)+16 {
			t.Fatalf("output grew beyond 2x the input: %d > %d (in=%q out=%q)", len(out), len(in), in, out)
		}
		if strings.TrimSpace(out) != out {
			t.Fatalf("output not trimmed: %q", out)
		}
		if strings.Contains(out, "\n\n\n") {
			t.Fatalf("output has more than one blank line in a row: %q", out)
		}
		for line := range strings.SplitSeq(out, "\n") {
			if strings.TrimSpace(line) != line {
				t.Fatalf("line not trimmed: %q in %q", line, out)
			}
		}
	})
}

// FuzzFindTrimmedLineMatches checks the match list is ordered, non-overlapping and inside
// the content, that each match really equals the searched lines modulo whitespace, and that
// a block cut out of the content is always found.
func FuzzFindTrimmedLineMatches(f *testing.F) {
	f.Add("a\nb\nc\n", "b", uint8(1), uint8(1))
	f.Add("  x\n\ty  \nz\n  x\n\ty  \n", "x\ny", uint8(0), uint8(2))
	f.Add("same\nsame\nsame\n", "same\nsame", uint8(0), uint8(2))
	f.Add("a\r\nb\r\nc\r\n", "a\nb", uint8(0), uint8(2))
	f.Add("", "x", uint8(0), uint8(1))
	f.Add("one", "", uint8(0), uint8(1))
	f.Add("日本\n語\n", " 日本 \n 語 ", uint8(0), uint8(2))

	f.Fuzz(func(t *testing.T, content, oldStr string, from, count uint8) {
		matches, lines := findTrimmedLineMatches(content, oldStr)
		if content == "" || oldStr == "" {
			if matches != nil || lines != nil {
				t.Fatalf("empty input must give no result, got %v %v", matches, lines)
			}
			return
		}
		checkMatches(t, content, oldStr, matches, lines)

		// A block of whole lines cut from the content must be found at or before its position.
		if len(lines) == 0 {
			return
		}
		start := int(from) % len(lines)
		end := min(start+1+int(count)%4, len(lines))
		block := strings.Join(lines[start:end], "\n")
		if strings.TrimRight(block, "\r\n") == "" || len(strings.Split(strings.TrimRight(block, "\r\n"), "\n")) != end-start {
			return // trailing blank lines are trimmed from the pattern, changing its height
		}
		found, _ := findTrimmedLineMatches(content, block)
		if len(found) == 0 || found[0].startLine > start+1 {
			t.Fatalf("block of lines %d..%d not found: matches=%v content=%q", start+1, end, found, content)
		}
	})
}

func checkMatches(t *testing.T, content, oldStr string, matches []lineMatch, lines []string) {
	t.Helper()
	target := strings.Split(strings.TrimRight(oldStr, "\r\n"), "\n")
	prevEnd := 0
	for i, m := range matches {
		if m.startLine < 1 || m.endLine < m.startLine || m.endLine > len(lines) {
			t.Fatalf("match %d has bad line range: %+v (content has %d lines)", i, m, len(lines))
		}
		if m.startLine <= prevEnd {
			t.Fatalf("match %d overlaps the previous one: %+v", i, matches)
		}
		prevEnd = m.endLine
		if m.endLine-m.startLine+1 != len(target) {
			t.Fatalf("match %d spans %d lines, pattern has %d", i, m.endLine-m.startLine+1, len(target))
		}
		if m.startByte < 0 || m.startByte > m.endByte || m.endByte > len(content) {
			t.Fatalf("match %d has bad byte range: %+v (len %d)", i, m, len(content))
		}
		got := strings.Split(content[m.startByte:m.endByte], "\n")
		if len(got) != len(target) {
			t.Fatalf("match %d byte range holds %d lines, want %d", i, len(got), len(target))
		}
		for j := range got {
			if strings.TrimSpace(strings.TrimRight(got[j], "\r")) != strings.TrimSpace(strings.TrimRight(target[j], "\r")) {
				t.Fatalf("match %d line %d = %q does not equal pattern line %q", i, j, got[j], target[j])
			}
		}
	}
}
