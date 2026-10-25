package diff_test

import (
	"testing"

	"hmj-agent/v2/internal/diff"
)

// FuzzDiffText ensures that diff calculations and hunk generation never panic
// on arbitrary content (multi-byte UTF-8, binary bytes, empty strings, mismatched lines).
func FuzzDiffText(f *testing.F) {
	f.Add("hello", "world")
	f.Add("line 1\nline 2\n", "line 1\nline 2 modified\n")
	f.Add("", "")
	f.Add("\x00\xff\xfe", "\xfe\xff\x00")
	f.Add("const a = 1\n", "const a = 2\n")
	f.Add("multi\nline\none\ntwo\nthree", "multi\nline\none\nmodified\nthree")

	f.Fuzz(func(t *testing.T, a, b string) {
		res := diff.Text("fuzz_target.txt", a, b, diff.Options{Context: 3, Inline: true})
		if res == nil {
			t.Fatal("expected non-nil diff.File")
		}
		_ = res.Empty()
		_ = res.TotalLines()
		_ = res.MaxNumber()
	})
}
