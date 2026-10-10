package tools

import "testing"

// Malformed markup found on real pages: unterminated comments and end tags, attributes
// without values, and unterminated or bare attribute values. The converter keeps the
// readable text and never panics.
func TestHTMLToTextMalformedMarkup(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"unterminated comment hides the rest", "kept<!-- never closed <p>gone", "kept"},
		{"script end tag without >", "a<script>x</script", "a"},
		{"boolean attribute before href", `<a download href="/f.zip">get</a>`, "[get](/f.zip)"},
		{"boolean attribute at the end", `<a href="/x" hidden>x</a>`, "[x](/x)"},
		{"bare value followed by more attributes", `<a href=/y class=c>y</a>`, "[y](/y)"},
		{"unterminated quoted value", `<a href="/z>z</a>`, ""},
		{"attribute named like href is not href", `<a hreflang=en>t</a>`, "t"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToText(tc.in); got != tc.want {
				t.Errorf("htmlToText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// attrValue reads double-quoted, single-quoted, bare and unterminated values, and
// skips attributes that have no value. (The tag scanner never hands it an unterminated
// quote, but attrValue stands on its own.)
func TestAttrValue(t *testing.T) {
	tests := []struct {
		attrs, want string
	}{
		{` href="/a"`, "/a"},
		{` class='x' href='/b'`, "/b"},
		{` download href=/c rel=x`, "/c"},
		{` href="/d`, "/d"},
		{` href = "/e&amp;f"`, "/e&f"},
		{` class=x`, ""},
		{` hidden`, ""},
		{``, ""},
	}
	for _, tc := range tests {
		if got := attrValue(tc.attrs, "href"); got != tc.want {
			t.Errorf("attrValue(%q) = %q, want %q", tc.attrs, got, tc.want)
		}
	}
}
