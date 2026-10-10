package terminal

import (
	"reflect"
	"strings"
	"testing"
)

func TestIsWideMiscSymbol(t *testing.T) {
	wide := []rune{
		0x2614, 0x2615, 0x2648, 0x2650, 0x2653, 0x267f, 0x2693, 0x26a1, 0x26aa, 0x26ab,
		0x26bd, 0x26be, 0x26c4, 0x26c5, 0x26ce, 0x26d4, 0x26ea, 0x26f2, 0x26f3, 0x26f5,
		0x26fa, 0x26fd, 0x2705, 0x270a, 0x270b, 0x2728, 0x274c, 0x274e, 0x2753, 0x2754,
		0x2755, 0x2757, 0x2795, 0x2796, 0x2797, 0x27b0, 0x27bf,
	}
	narrow := []rune{
		0x2613, 0x2616, 0x2647, 0x2654, 0x267e, 0x2680, 0x2692, 0x2694, 0x26a0, 0x26a2,
		0x26a9, 0x26ac, 0x26bc, 0x26bf, 0x26c3, 0x26c6, 0x26cd, 0x26cf, 0x26d3, 0x26d5,
		0x26e9, 0x26eb, 0x26f1, 0x26f4, 0x26f6, 0x26f9, 0x26fb, 0x26fc, 0x26fe, 0x2704,
		0x2706, 0x2709, 0x270c, 0x2727, 0x2729, 0x274b, 0x274d, 0x274f, 0x2752, 0x2756,
		0x2758, 0x2794, 0x2798, 0x27af, 0x27b1, 0x27be, 'a', 0,
	}
	for _, r := range wide {
		if !isWideMiscSymbol(r) {
			t.Errorf("U+%04X should be wide", r)
		}
		if runeWidth(r) != 2 {
			t.Errorf("runeWidth(U+%04X) = %d, want 2", r, runeWidth(r))
		}
	}
	for _, r := range narrow {
		if isWideMiscSymbol(r) {
			t.Errorf("U+%04X should not be wide", r)
		}
	}
	// A narrow symbol inside the Misc Symbols block still takes one column.
	if runeWidth(0x2600) != 1 {
		t.Errorf("runeWidth(U+2600) = %d, want 1", runeWidth(0x2600))
	}
}

func TestSplitCellWordByWidth(t *testing.T) {
	tests := []struct {
		name      string
		word      string
		width     int
		want      []string
		wantLastW int
	}{
		{"empty", "", 5, nil, 0},
		{"fits exactly", "abc", 3, []string{"abc"}, 3},
		{"remainder chunk", "abcdefg", 3, []string{"abc", "def", "g"}, 1},
		{"width one", "abc", 1, []string{"a", "b", "c"}, 1},
		{"zero width still progresses", "abc", 0, []string{"a", "b", "c"}, 1},
		{"CJK pairs", "日本語", 4, []string{"日本", "語"}, 2},
		{"CJK odd width", "日本語", 3, []string{"日", "本", "語"}, 2},
		{"wide rune wider than column", "日", 1, []string{"日"}, 2},
		{"mixed narrow and wide", "a日b", 2, []string{"a", "日", "b"}, 1},
		{"emoji", "😀😀😀", 4, []string{"😀😀", "😀"}, 2},
		{"narrow after wide shares chunk", "日ab", 4, []string{"日ab"}, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chunks, lastW := splitCellWord(cellLine{{tc.word, stylePlain}}, tc.width)
			var got []string
			if chunks != nil {
				got = plainLines(chunks)
			}
			if !reflect.DeepEqual(got, tc.want) || lastW != tc.wantLastW {
				t.Errorf("splitCellWord(%q, %d) = %q, %d; want %q, %d", tc.word, tc.width, got, lastW, tc.want, tc.wantLastW)
			}
		})
	}
}

func TestWrapParagraphBreaksLongWordsByWidth(t *testing.T) {
	got := plainLines(wrapParagraph("see https://example.com/very/long/path ok", 10))
	want := []string{"see", "https://ex", "ample.com/", "very/long/", "path ok"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wrapParagraph = %q, want %q", got, want)
	}
	for _, line := range got {
		if VisibleWidth(line) > 10 {
			t.Errorf("line %q is wider than 10", line)
		}
	}
}

func TestScaleColumnWidths(t *testing.T) {
	tests := []struct {
		name  string
		cols  []int
		avail int
		want  []int
	}{
		{"proportional halves", []int{40, 40}, 40, []int{20, 20}},
		{"equal thirds", []int{10, 10, 10}, 60, []int{20, 20, 20}},
		{"leftover goes to leading columns", []int{30, 30, 30}, 100, []int{34, 33, 33}},
		{"narrow column floored at 8", []int{100, 10}, 50, []int{45, 8}},
		{"zero natural widths use the floor", []int{0, 0}, 20, []int{9, 9}},
		{"single column", []int{20}, 10, []int{10}},
		{"leftover larger than columns is dropped", []int{0, 0}, 40, []int{9, 9}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scaleColumnWidths(tc.cols, tc.avail)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("scaleColumnWidths(%v, %d) = %v, want %v", tc.cols, tc.avail, got, tc.want)
			}
		})
	}
}

// A reply that ends in a table row without a trailing newline must still print that row:
// Flush used to buffer it as a table line without opening the table.
func TestTableStreamerFlushKeepsFinalTableRow(t *testing.T) {
	var out strings.Builder
	s := NewTableStreamer(func(text string) { out.WriteString(text) })
	s.Write("| only | row |")
	s.Flush()
	if !strings.Contains(out.String(), "only") || !strings.Contains(out.String(), "row") {
		t.Errorf("final table row was dropped; output %q", out.String())
	}

	out.Reset()
	s = NewTableStreamer(func(text string) { out.WriteString(text) })
	s.Write("| a | b |\n|---|---|\n| 1 | 2 |")
	s.Flush()
	if !strings.Contains(out.String(), "1") || !strings.Contains(out.String(), "2") {
		t.Errorf("last row of a table without a trailing newline was dropped; output %q", out.String())
	}
}
