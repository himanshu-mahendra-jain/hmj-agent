package agent

import (
	"fmt"
	"strings"
	"time"

	"hmj-agent/v2/internal/terminal/theme"
)

// maxThoughtBuffer bounds the reasoning a thoughtLine keeps; only the latest line is shown.
const maxThoughtBuffer = 4096

// thoughtLine follows the model's streamed reasoning for the status line.
type thoughtLine struct {
	start time.Time
	buf   string // the latest reasoning text
}

// add appends a chunk of reasoning and returns the sentence the model is writing, or the last
// one it finished when a new one has not begun.
func (t *thoughtLine) add(chunk string) string {
	t.buf += chunk
	if len(t.buf) > maxThoughtBuffer {
		t.buf = strings.ToValidUTF8(t.buf[len(t.buf)-maxThoughtBuffer/2:], "")
	}
	return latestSentence(t.buf)
}

// latestSentence returns the last sentence of text's last non-blank line, without Markdown
// bold markers and made safe for the status line.
func latestSentence(text string) string {
	lines := strings.Split(text, "\n")
	line := ""
	for i := len(lines) - 1; i >= 0 && line == ""; i-- {
		line = progressLine(strings.ReplaceAll(lines[i], "**", ""))
	}
	cut := 0
	for i := 0; i+1 < len(line); i++ {
		if strings.IndexByte(".?!", line[i]) >= 0 && line[i+1] == ' ' {
			cut = i + 2
		}
	}
	return line[cut:]
}

// printThoughtDuration prints the line that stands in the transcript for the model's reasoning:
// how long it took, in whole seconds, at least 1.
func printThoughtDuration(d time.Duration) {
	sec := max(1, int(d.Round(time.Second).Seconds()))
	took := fmt.Sprintf("%ds", sec)
	if sec >= 60 {
		took = fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	fmt.Printf("%s\n", theme.Thinking()+theme.ASCII("▸ ")+"Thought for "+took+theme.Reset())
}
