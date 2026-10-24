package tools

import (
	"context"
	"strings"
)

// progressKey is the context key for the function a running tool reports progress to.
type progressKey struct{}

// WithProgress returns a copy of ctx that carries report. A long-running tool calls it with
// the latest line of its output, so the terminal can show the tool is still working.
func WithProgress(ctx context.Context, report func(line string)) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

// progressFrom returns the report function ctx carries, or nil.
func progressFrom(ctx context.Context) func(string) {
	report, _ := ctx.Value(progressKey{}).(func(string))
	return report
}

// maxProgressLineBytes is how much of one output line is kept for progress reports. The
// terminal shows the start of the line, cut to its width.
const maxProgressLineBytes = 1024

// lastLineTracker follows the last line of a stream for progress reports. The line being
// written counts once it has text, so output that grows without a newline (a test runner's
// dots) shows as it grows. A carriage return starts the line again, as progress bars redraw
// it; CRLF still ends a line.
type lastLineTracker struct {
	partial []byte
	cr      bool
	last    string
}

// Write follows p and returns the line to report: the line being written if it has text,
// else the last complete line with text.
func (t *lastLineTracker) Write(p []byte) string {
	for _, b := range p {
		switch b {
		case '\n':
			if line := strings.TrimSpace(string(t.partial)); line != "" {
				t.last = line
			}
			t.partial = t.partial[:0]
			t.cr = false
		case '\r':
			t.cr = true
		default:
			if t.cr {
				t.partial = t.partial[:0]
				t.cr = false
			}
			if len(t.partial) < maxProgressLineBytes {
				t.partial = append(t.partial, b)
			}
		}
	}
	if line := strings.TrimSpace(string(t.partial)); line != "" {
		return strings.ToValidUTF8(line, "")
	}
	return strings.ToValidUTF8(t.last, "")
}
