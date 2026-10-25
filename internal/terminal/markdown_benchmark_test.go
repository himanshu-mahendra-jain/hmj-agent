package terminal

import (
	"strings"
	"testing"

	"hmj-agent/v2/internal/terminal/theme"
)

// benchmarkAnswer is about 100 KB of the Markdown a model writes: headings, prose with
// bold, italic and code, bullets, a numbered list, a quote, a code fence and a table.
func benchmarkAnswer() string {
	section := `## Findings for the **storage** layer

The session file is rewritten on every turn, and *each* write calls ` + "`fsync`" + ` before the rename, so a crash never leaves half a file behind. This paragraph is long enough to wrap several times at the default width of the terminal, which is what the renderer spends most of its time on.

- First, the **manager** keeps every conversation in memory and marshals all of them.
- Second, the ` + "`Save`" + ` call holds the lock for the whole write, including the disk sync.
  - A nested item that also needs wrapping because it goes on for quite a while longer than one row.
1. Measure before changing anything.
2. Change one thing at a time and keep what is faster.

> A quote from the design notes, kept short.

` + "```go" + `
func (sm *SessionManager) Save() error {
	return sm.write()
}
` + "```" + `

| Case | Before | After |
|------|-------:|------:|
| small | 0.2 ms | 0.2 ms |
| large | 1.6 ms | 1.5 ms |

`
	return strings.Repeat(section, 100*1024/len(section)+1)
}

// BenchmarkMarkdownStream100KB streams a 100 KB answer in 4-byte chunks, as a model's
// tokens arrive, through the renderer with colour, margin and wrapping on.
func BenchmarkMarkdownStream100KB(b *testing.B) {
	answer := benchmarkAnswer()
	prevPad := padMarkdown
	padMarkdown = func() bool { return true }
	theme.SetEnabled(true)
	b.Cleanup(func() {
		padMarkdown = prevPad
		theme.SetEnabled(false)
	})
	b.SetBytes(int64(len(answer)))
	b.ReportAllocs()
	for b.Loop() {
		n := 0
		s := NewMarkdownStreamer(func(out string) { n += len(out) })
		for i := 0; i < len(answer); i += 4 {
			s.Write(answer[i:min(i+4, len(answer))])
		}
		s.Flush()
		if n == 0 {
			b.Fatal("nothing rendered")
		}
	}
}
