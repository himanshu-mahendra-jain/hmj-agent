package diff

import "strings"

// Plain renders the diff as an escape-free string for test assertions.
func (f *File) Plain() string {
	var b strings.Builder
	for _, r := range f.Render(nil) {
		line := r.Number + r.Glyph + r.Body
		if line == "" {
			line = r.Raw
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
