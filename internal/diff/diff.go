// Package diff computes line-oriented diffs with real file line numbers and
// word-level emphasis for the changed runs inside a modified line.
//
// It is dependency-free and colour-free: it produces a structured []Line model
// that the display layer paints via a PaintFunc. Keeping the algorithm pure
// makes it cheap to test and lets the same result drive terminal output and
// plain-text summaries.
package diff

import (
	"strings"
)

// Kind classifies a rendered diff line.
type Kind int

const (
	Ctx Kind = iota // unchanged context
	Del             // removed line
	Add             // added line
)

// Seg is a run of text inside a line. Changed runs are the ones an inline
// (word-level) comparison flagged as actually differing.
type Seg struct {
	Text    string
	Changed bool
}

// Line is one row of a diff. Old/New are 1-based file line numbers; 0 means the
// side has no line there (pure addition/removal).
type Line struct {
	Kind Kind
	Old  int
	New  int
	Text string
	Segs []Seg // optional inline breakdown of Text
}

// Hunk is a contiguous run of lines under one @@ header.
type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []Line
}

// File is the diff of one file's old content versus its new content.
type File struct {
	Path    string
	Hunks   []Hunk
	Added   int
	Removed int
	// Omitted counts rows dropped by MaxLines truncation.
	Omitted int
	// Truncated is set when MaxLines cut the render short.
	Truncated bool
	// NewlineMissingOld/New are the "\ No newline at end of file" flags.
	NewlineMissingOld bool
	NewlineMissingNew bool
}

// Empty reports whether nothing changed.
func (f *File) Empty() bool { return f == nil || (f.Added == 0 && f.Removed == 0) }

// TotalLines is the number of rendered rows across all hunks.
func (f *File) TotalLines() int {
	n := 0
	for _, h := range f.Hunks {
		n += len(h.Lines)
	}
	return n
}

// Options controls Text().
type Options struct {
	Context  int  // unchanged lines kept around each change (default 3)
	MaxLines int  // hard cap on rendered rows; 0 = no cap
	Inline   bool // compute word-level Segs for changed pairs
}

// DefaultContext is the number of unchanged lines shown around a change.
const DefaultContext = 3

// lcsCellBudget bounds the DP table size before falling back to block mode.
const lcsCellBudget = 90000

// Text computes the diff between two versions of a file's content.
func Text(path, oldText, newText string, o Options) *File {
	if o.Context <= 0 {
		o.Context = DefaultContext
	}

	f := &File{Path: path}
	if oldText == newText {
		return f
	}

	f.NewlineMissingOld = oldText != "" && !strings.HasSuffix(oldText, "\n")
	f.NewlineMissingNew = newText != "" && !strings.HasSuffix(newText, "\n")

	a := splitLines(oldText)
	b := splitLines(newText)

	// Trim the common prefix and suffix so the quadratic LCS only runs over the
	// region that actually differs (the overwhelmingly common case for surgical
	// edits) while keeping real line numbers exact.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}

	ops := diffMiddle(a[pre:len(a)-suf], b[pre:len(b)-suf], pre+1)

	lines := make([]Line, 0, pre+len(ops)+suf)
	for i := range pre {
		lines = append(lines, Line{Kind: Ctx, Old: i + 1, New: i + 1, Text: a[i]})
	}
	lines = append(lines, ops...)
	for i := range suf {
		k := len(a) - suf + i
		m := len(b) - suf + i
		lines = append(lines, Line{Kind: Ctx, Old: k + 1, New: m + 1, Text: a[k]})
	}

	for _, l := range lines {
		switch l.Kind {
		case Add:
			f.Added++
		case Del:
			f.Removed++
		}
	}

	if o.Inline {
		pairInline(lines)
	}

	f.buildHunks(lines, o.Context)
	if o.MaxLines > 0 {
		f.cap(o.MaxLines)
	}
	return f
}

// diffMiddle produces Del/Add/Ctx rows for the differing middle sections.
// startLine is the 1-based file line number of the first row in either slice.
func diffMiddle(a, b []string, startLine int) []Line {
	if len(a)*len(b) <= lcsCellBudget {
		return lcsLines(a, b, startLine)
	}
	return blockLines(a, b, startLine)
}

// lcsLines walks a longest-common-subsequence table to emit the classic
// delete-then-add interleave for the changed region.
func lcsLines(a, b []string, start int) []Line {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:], b[j:]
	dp := make([]int32, (n+1)*(m+1))
	stride := m + 1
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i*stride+j] = dp[(i+1)*stride+j+1] + 1
			case dp[(i+1)*stride+j] >= dp[i*stride+j+1]:
				dp[i*stride+j] = dp[(i+1)*stride+j]
			default:
				dp[i*stride+j] = dp[i*stride+j+1]
			}
		}
	}

	out := make([]Line, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, Line{Kind: Ctx, Old: start + i, New: start + j, Text: a[i]})
			i++
			j++
		case dp[(i+1)*stride+j] >= dp[i*stride+j+1]:
			out = append(out, Line{Kind: Del, Old: start + i, Text: a[i]})
			i++
		default:
			out = append(out, Line{Kind: Add, New: start + j, Text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, Line{Kind: Del, Old: start + i, Text: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, Line{Kind: Add, New: start + j, Text: b[j]})
	}
	return out
}

// blockLines is the fallback for huge changes: everything removed, then
// everything added. Numbers stay correct, only the interleaving is lost.
func blockLines(a, b []string, start int) []Line {
	out := make([]Line, 0, len(a)+len(b))
	for i := range a {
		out = append(out, Line{Kind: Del, Old: start + i, Text: a[i]})
	}
	for j := range b {
		out = append(out, Line{Kind: Add, New: start + j, Text: b[j]})
	}
	return out
}

// splitLines splits content into lines without their terminators. An empty
// string yields no lines (a zero-byte file diffs as "nothing here").
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	s := strings.Split(content, "\n")
	if s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

// pairInline walks Del/Add runs and attaches word-level Segs so the renderer
// can highlight exactly which words changed within otherwise similar rows.
func pairInline(lines []Line) {
	for i := 0; i < len(lines); i++ {
		if lines[i].Kind != Del {
			continue
		}
		dStart := i
		for i+1 < len(lines) && lines[i+1].Kind == Del {
			i++
		}
		dEnd := i
		aStart := i + 1
		aEnd := aStart
		for aEnd < len(lines) && lines[aEnd].Kind == Add {
			aEnd++
		}
		if aStart >= aEnd {
			continue
		}

		dels := lines[dStart : dEnd+1]
		adds := lines[aStart:aEnd]

		// Pair rows by position; 1:1 pairs are what make inline marks useful.
		n := min(len(dels), len(adds))
		for k := range n {
			if similar(dels[k].Text, adds[k].Text) {
				markInline(&dels[k], &adds[k])
			}
		}
	}
}

// similar reports whether two lines are close enough that a word diff is more
// informative than two block rows (at least half the shorter line is shared).
func similar(x, y string) bool {
	if x == y {
		return false
	}
	rx, ry := strings.TrimSpace(x), strings.TrimSpace(y)
	if rx == "" || ry == "" {
		return false
	}
	shared := 0
	total := min(len(rx), len(ry))
	for i := range total {
		if rx[i] == ry[i] {
			shared++
			continue
		}
		break
	}
	return shared*2 >= total
}

// markInline computes word-level segments for a removed/added pair.
func markInline(del, add *Line) {
	dw, aw := tokens(del.Text), tokens(add.Text)
	if dw == nil || aw == nil || len(dw)*len(aw) > lcsCellBudget {
		return
	}
	del.Segs = joinSegs(missingMarks(dw, aw))
	add.Segs = joinSegs(missingMarks(aw, dw))
}

// missingMarks flags tokens of a that do not appear (with multiplicity) in b.
func missingMarks(a, b []string) []Seg {
	present := make(map[string]int, len(b))
	for _, t := range b {
		present[t]++
	}
	out := make([]Seg, 0, len(a))
	for _, t := range a {
		changed := false
		if present[t] == 0 {
			changed = true
		} else {
			present[t]--
		}
		out = append(out, Seg{Text: t, Changed: changed})
	}
	return out
}

// joinSegs merges adjacent segments with the same changed flag.
func joinSegs(segs []Seg) []Seg {
	var out []Seg
	for _, s := range segs {
		if len(out) > 0 && out[len(out)-1].Changed == s.Changed {
			out[len(out)-1].Text += s.Text
			continue
		}
		out = append(out, Seg{Text: s.Text, Changed: s.Changed})
	}
	return out
}

// tokens splits a line into word / whitespace / single-punctuation runs.
// The split is lossless: concatenating the tokens reproduces the input.
func tokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	i := 0
	for i < len(s) {
		j := i
		switch {
		case isSpaceByte(s[i]):
			for j < len(s) && isSpaceByte(s[j]) {
				j++
			}
		case isWordByte(s[i]):
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
		default:
			j = i + 1
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\v' || c == '\f' }

func isWordByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c >= 0x80
}

// buildHunks groups lines into context-delimited hunks.
func (f *File) buildHunks(lines []Line, context int) {
	changed := make([]int, 0, len(lines))
	for i, l := range lines {
		if l.Kind != Ctx {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return
	}

	// Mark the context window around each change.
	keep := make([]bool, len(lines))
	for _, ci := range changed {
		lo := max(0, ci-context)
		hi := min(len(lines)-1, ci+context)
		for k := lo; k <= hi; k++ {
			keep[k] = true
		}
	}

	// Group contiguous kept ranges, merging neighbours that share a window.
	var groups [][2]int
	start := -1
	for i := range lines {
		if keep[i] {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			groups = append(groups, [2]int{start, i - 1})
			start = -1
		}
	}
	if start >= 0 {
		groups = append(groups, [2]int{start, len(lines) - 1})
	}

	merged := make([][2]int, 0, len(groups))
	for _, g := range groups {
		if n := len(merged); n > 0 && g[0]-merged[n-1][1] <= context {
			merged[n-1][1] = g[1]
			continue
		}
		merged = append(merged, g)
	}

	for _, g := range merged {
		h := Hunk{}
		for i := g[0]; i <= g[1]; i++ {
			l := lines[i]
			if h.OldStart == 0 {
				h.OldStart = firstNonZero(l.Old, l.New)
			}
			if h.NewStart == 0 {
				h.NewStart = firstNonZero(l.New, l.Old)
			}
			if l.Kind != Add {
				h.OldCount++
			}
			if l.Kind != Del {
				h.NewCount++
			}
			h.Lines = append(h.Lines, l)
		}
		h.OldStart = firstNonZero(h.OldStart, 1)
		h.NewStart = firstNonZero(h.NewStart, 1)
		f.Hunks = append(f.Hunks, h)
	}
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// cap trims rendered rows to maxLines, oldest hunks first, recording what was
// dropped in Omitted so the renderer can say "… 142 more lines".
func (f *File) cap(maxLines int) {
	total := f.TotalLines()
	if total <= maxLines {
		return
	}

	used := 0
	for i, h := range f.Hunks {
		if used+len(h.Lines) <= maxLines {
			used += len(h.Lines)
			continue
		}

		if room := maxLines - used; room > 0 {
			kept := h.Lines[:room]
			var oldCount, newCount int
			for _, l := range kept {
				if l.Kind != Add {
					oldCount++
				}
				if l.Kind != Del {
					newCount++
				}
			}
			f.Hunks = f.Hunks[:i+1]
			f.Hunks[i].Lines = kept
			f.Hunks[i].OldCount = oldCount
			f.Hunks[i].NewCount = newCount
		} else {
			f.Hunks = f.Hunks[:i]
		}

		f.Omitted = total - maxLines
		f.Truncated = true
		return
	}
}

// MaxNumber returns the gutter width needed for the largest line number shown.
func (f *File) MaxNumber() int {
	max := 1
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Old > max {
				max = l.Old
			}
			if l.New > max {
				max = l.New
			}
		}
	}
	return digits(max)
}

func digits(n int) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}
