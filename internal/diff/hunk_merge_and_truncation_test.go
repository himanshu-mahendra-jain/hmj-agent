package diff

import (
	"fmt"
	"strings"
	"testing"
)

// numbered returns n lines "line 1" … "line n", with the lines in change replaced by "CHANGED k".
func numbered(n int, change ...int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		text := fmt.Sprintf("line %d", i)
		for _, c := range change {
			if c == i {
				text = fmt.Sprintf("CHANGED %d", i)
			}
		}
		b.WriteString(text + "\n")
	}
	return b.String()
}

// hunkCountsMatch checks that a hunk's header counts agree with its lines.
func hunkCountsMatch(t *testing.T, h Hunk) {
	t.Helper()
	var oldN, newN int
	for _, l := range h.Lines {
		if l.Kind != Add {
			oldN++
		}
		if l.Kind != Del {
			newN++
		}
	}
	if oldN != h.OldCount || newN != h.NewCount {
		t.Errorf("hunk %s has %d old and %d new lines", h.HeaderOf(), oldN, newN)
	}
}

// TestNearbyChangesShareOneHunk checks that two changes whose context windows are separated by
// fewer lines than the context size are shown as one hunk, gap included, while changes further
// apart get their own hunks.
func TestNearbyChangesShareOneHunk(t *testing.T) {
	tests := []struct {
		name      string
		changes   []int
		wantHunks int
	}{
		{"one line between the windows", []int{5, 13}, 1},
		{"two lines between the windows", []int{5, 14}, 1},
		{"a context's worth between the windows", []int{5, 15}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Text("f", numbered(30), numbered(30, tc.changes...), Options{Context: 3})
			if len(d.Hunks) != tc.wantHunks {
				t.Fatalf("hunks = %d, want %d:\n%s", len(d.Hunks), tc.wantHunks, d.Plain())
			}
			for _, h := range d.Hunks {
				hunkCountsMatch(t, h)
			}
			if tc.wantHunks == 1 {
				gap := fmt.Sprintf("line %d", tc.changes[0]+4)
				if !strings.Contains(d.Plain(), gap) {
					t.Errorf("merged hunk is missing the gap line %q:\n%s", gap, d.Plain())
				}
			}
		})
	}
}

// TestPureDeletionRunHasNoInlineMarks checks that consecutive removed lines with nothing added in
// their place are shown as whole removed rows, without word marks.
func TestPureDeletionRunHasNoInlineMarks(t *testing.T) {
	d := Text("f", "a\nb\nc\nd\n", "a\nd\n", Options{Inline: true})
	if d.Removed != 2 || d.Added != 0 {
		t.Fatalf("removed %d, added %d; want 2, 0", d.Removed, d.Added)
	}
	for _, l := range d.Hunks[0].Lines {
		if len(l.Segs) != 0 {
			t.Errorf("line %+v has inline marks", l)
		}
	}
}

// TestInlineMarksSkipUnhelpfulPairs checks the pairs that get no word marks: a blank line replaced
// by text, a line that is paired with an identical line by the block fallback, and lines too long
// for the word comparison budget.
func TestInlineMarksSkipUnhelpfulPairs(t *testing.T) {
	t.Run("blank line", func(t *testing.T) {
		d := Text("f", "x\n   \ny\n", "x\nnew text\ny\n", Options{Inline: true})
		for _, l := range d.Hunks[0].Lines {
			if len(l.Segs) != 0 {
				t.Errorf("line %q has inline marks", l.Text)
			}
		}
	})

	t.Run("identical pair from the block fallback", func(t *testing.T) {
		// 301 x 301 differing lines exceed the LCS budget, so rows pair by position.
		var a, b strings.Builder
		for i := range 301 {
			if i == 5 {
				a.WriteString("same line\n")
				b.WriteString("same line\n")
				continue
			}
			fmt.Fprintf(&a, "old %d\n", i)
			fmt.Fprintf(&b, "new %d\n", i)
		}
		d := Text("f", a.String(), b.String(), Options{Inline: true})
		for _, h := range d.Hunks {
			for _, l := range h.Lines {
				if l.Text == "same line" && len(l.Segs) != 0 {
					t.Errorf("identical line %+v has inline marks", l)
				}
			}
		}
	})

	t.Run("very long similar lines", func(t *testing.T) {
		long := strings.Repeat("word ", 200)
		d := Text("f", long+"end\n", long+"finish\n", Options{Inline: true})
		for _, l := range d.Hunks[0].Lines {
			if len(l.Segs) != 0 {
				t.Errorf("line of %d bytes has inline marks", len(l.Text))
			}
		}
	})
}

// TestMaxLinesCutsBetweenHunks checks truncation across hunks: hunks that fit are kept whole, the
// hunk at the limit is cut with header counts that match the rows kept, and a hunk that starts at
// the limit is dropped entirely.
func TestMaxLinesCutsBetweenHunks(t *testing.T) {
	old := numbered(40)
	cur := numbered(40, 5, 25)
	full := Text("f", old, cur, Options{Context: 1})
	if len(full.Hunks) != 2 {
		t.Fatalf("setup: hunks = %d, want 2", len(full.Hunks))
	}
	first := len(full.Hunks[0].Lines) // ctx, del, add, ctx

	t.Run("cut inside the second hunk", func(t *testing.T) {
		d := Text("f", old, cur, Options{Context: 1, MaxLines: first + 3})
		if len(d.Hunks) != 2 || d.TotalLines() != first+3 || !d.Truncated || d.Omitted != 1 {
			t.Fatalf("hunks %d, rows %d, truncated %v, omitted %d; want 2, %d, true, 1", len(d.Hunks), d.TotalLines(), d.Truncated, d.Omitted, first+3)
		}
		last := d.Hunks[1]
		hunkCountsMatch(t, last)
		if last.Lines[len(last.Lines)-1].Kind != Add {
			t.Errorf("kept rows of the cut hunk = %+v, want them to end with the added line", last.Lines)
		}
		if !strings.Contains(d.Plain(), "… 1 more changed lines not shown") {
			t.Errorf("render is missing the truncation note:\n%s", d.Plain())
		}
	})

	t.Run("limit at a hunk boundary", func(t *testing.T) {
		d := Text("f", old, cur, Options{Context: 1, MaxLines: first})
		if len(d.Hunks) != 1 || d.Omitted != len(full.Hunks[1].Lines) {
			t.Errorf("hunks %d, omitted %d; want 1 and the whole second hunk", len(d.Hunks), d.Omitted)
		}
	})
}

// TestRenderEdgeCases covers the rows Render adds around the hunks.
func TestRenderEdgeCases(t *testing.T) {
	var nilFile *File
	if rows := nilFile.Render(nil); rows != nil {
		t.Errorf("nil file renders %d rows", len(rows))
	}

	d := Text("f", "a\nb", "a\nc", Options{})
	plain := d.Plain()
	if !strings.HasSuffix(plain, "\\ No newline at end of file\n") {
		t.Errorf("render is missing the no-newline note:\n%s", plain)
	}

	if got := (&File{}).StatSummary(); got != "no changes" {
		t.Errorf("StatSummary of an empty diff = %q", got)
	}
}
