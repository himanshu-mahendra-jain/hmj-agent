package terminal

import (
	"strings"
	"testing"
)

// FuzzUnquoteDragPath checks drag-and-drop path cleanup never grows the text, leaves plain
// paths alone and strips exactly one pair of matching quotes.
func FuzzUnquoteDragPath(f *testing.F) {
	f.Add("/tmp/file.txt")
	f.Add("'/tmp/my file.txt'")
	f.Add("\"/tmp/my file.txt\"")
	f.Add("/tmp/my\\ file.txt")
	f.Add("file:///tmp/a%20b.txt")
	f.Add("file://localhost/tmp/x")
	f.Add("file://host")
	f.Add("file:///tmp/%zz%4")
	f.Add("'it's'")
	f.Add("  /padded  \n")
	f.Add("''")
	f.Add("%")

	f.Fuzz(func(t *testing.T, in string) {
		out := UnquoteDragPath(in)
		if len(out) > len(in) {
			t.Fatalf("output longer than input: %q -> %q", in, out)
		}
		trimmed := strings.TrimSpace(in)
		if !strings.ContainsAny(trimmed, "'\"\\%") && !strings.HasPrefix(trimmed, "file://") && out != trimmed {
			t.Fatalf("plain path changed: %q -> %q", in, out)
		}
		if n := len(trimmed); n >= 2 && trimmed[0] == '\'' && trimmed[n-1] == '\'' && !strings.Contains(trimmed[1:n-1], "'") &&
			!strings.HasPrefix(trimmed, "file://") && out != trimmed[1:n-1] {
			t.Fatalf("single quotes not stripped: %q -> %q", in, out)
		}
	})
}
