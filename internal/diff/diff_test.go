package diff

import (
	"strings"
	"testing"
)

func TestDiffBasicAddRemove(t *testing.T) {
	oldText := "alpha\nbeta\ngamma\ndelta\n"
	newText := "alpha\nBETA\ngamma\ndelta\n"

	d := Text("f.txt", oldText, newText, Options{})
	if d.Added != 1 || d.Removed != 1 {
		t.Fatalf("expected +1/−1, got +%d/−%d", d.Added, d.Removed)
	}
	if d.Truncated || d.Omitted != 0 {
		t.Fatalf("unexpected truncation")
	}

	var dels, adds []Line
	for _, h := range d.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case Del:
				dels = append(dels, l)
			case Add:
				adds = append(adds, l)
			}
		}
	}
	if len(dels) != 1 || dels[0].Text != "beta" {
		t.Fatalf("expected one removed 'beta', got %#v", dels)
	}
	if len(adds) != 1 || adds[0].Text != "BETA" {
		t.Fatalf("expected one added 'BETA', got %#v", adder(adds))
	}
	if dels[0].Old != 2 {
		t.Fatalf("removed line should carry old number 2, got %d", dels[0].Old)
	}
	if adds[0].New != 2 {
		t.Fatalf("added line should carry new number 2, got %d", adds[0].New)
	}
}

func adder(adds []Line) string {
	if len(adds) == 0 {
		return ""
	}
	return adds[0].Text
}

func TestDiffLineNumbers(t *testing.T) {
	oldText := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	newText := "one\ntwo\nthree\nfour\nFIVE\nsix\nseven\neight\nnine\nten\neleven\n"

	d := Text("f.txt", oldText, newText, Options{Inline: true})
	for _, h := range d.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case Del:
				if l.Old != 5 || l.New != 0 {
					t.Fatalf("del at wrong number: old=%d new=%d", l.Old, l.New)
				}
			case Add:
				if l.Text == "FIVE" && l.New != 5 {
					t.Fatalf("FIVE should be new line 5, got %d", l.New)
				}
				if l.Text == "eleven" && l.New != 11 {
					t.Fatalf("eleven should be new line 11, got %d", l.New)
				}
			}
		}
	}
}

func TestDiffInlineMarks(t *testing.T) {
	d := Text("f.txt", "hello world\n", "hello brave world\n", Options{Inline: true})
	if len(d.Hunks) != 1 || len(d.Hunks[0].Lines) < 2 {
		t.Fatalf("expected one hunk with del+add, got %#v", d.Hunks)
	}
	var del, add *Line
	for i := range d.Hunks[0].Lines {
		l := &d.Hunks[0].Lines[i]
		switch l.Kind {
		case Del:
			del = l
		case Add:
			add = l
		}
	}
	if del == nil || add == nil {
		t.Fatalf("expected both del and add lines")
	}
	if len(add.Segs) == 0 {
		t.Fatalf("expected inline segments on added line")
	}
	found := false
	for _, s := range add.Segs {
		if s.Changed && (strings.Contains(s.Text, "brave")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'brave' marked as changed, segs=%#v", add.Segs)
	}
}

func TestDiffIdenticalAndEmpty(t *testing.T) {
	if d := Text("f", "same\n", "same\n", Options{}); !d.Empty() {
		t.Fatalf("identical content should produce empty diff")
	}
	if d := Text("f", "", "one\n", Options{}); d.Empty() {
		t.Fatalf("empty→content should be non-empty")
	}
	if d := Text("f", "one\n", "", Options{}); d.Empty() {
		t.Fatalf("content→empty should be non-empty")
	}
}

func TestDiffNoNewlineFlags(t *testing.T) {
	d := Text("f", "abc", "abc\n", Options{})
	if !d.NewlineMissingOld || d.NewlineMissingNew {
		t.Fatalf("old side should be flagged missing newline: %+v", d)
	}
}

func TestDiffContextGrouping(t *testing.T) {
	old := ""
	for i := 1; i <= 30; i++ {
		old += strings.Repeat("x", i) + "\n"
	}
	newText := ""
	for i := 1; i <= 30; i++ {
		if i == 10 || i == 25 {
			newText += strings.Repeat("X", i) + "\n"
			continue
		}
		newText += strings.Repeat("x", i) + "\n"
	}

	d := Text("f", old, newText, Options{Context: 2})
	if len(d.Hunks) != 2 {
		t.Fatalf("two distant changes should yield two hunks, got %d", len(d.Hunks))
	}
	for _, h := range d.Hunks {
		if h.OldStart < 1 || h.NewStart < 1 {
			t.Fatalf("hunk start must be 1-based, got %d/%d", h.OldStart, h.NewStart)
		}
	}
}

func TestDiffMaxLinesCap(t *testing.T) {
	var old, cur strings.Builder
	for range 200 {
		old.WriteString("line\n")
		cur.WriteString("changed\n")
	}
	d := Text("f", old.String(), cur.String(), Options{MaxLines: 10})
	if !d.Truncated {
		t.Fatalf("expected truncation flag")
	}
	if got := d.TotalLines(); got > 10 {
		t.Fatalf("expected ≤10 rendered rows, got %d", got)
	}
	if d.Omitted == 0 {
		t.Fatalf("expected non-zero omitted count")
	}
}

func TestDiffRenderRows(t *testing.T) {
	d := Text("numbers.go", "a\nb\nc\n", "a\nB\nc\n", Options{Inline: false})
	rows := d.Render(nil)
	plain := d.Plain()

	if !strings.Contains(plain, "numbers.go") {
		t.Fatalf("plain render missing path header:\n%s", plain)
	}
	var sawDel, sawAdd bool
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.Raw, "2 ") && strings.Contains(r.Raw, "- b"):
			sawDel = true
		case strings.HasSuffix(r.Raw, "+ B"):
			sawAdd = true
		}
	}
	if !sawDel {
		t.Fatalf("expected a deleted row with old line number 2:\n%s", plain)
	}
	if !sawAdd {
		t.Fatalf("expected an added row:\n%s", plain)
	}
}

func TestDiffRenderPaintStyles(t *testing.T) {
	d := Text("f", "x\n", "y\n", Options{})
	styles := map[SegmentStyle]int{}
	rows := d.Render(func(s string, k SegmentStyle) string {
		styles[k]++
		return s
	})
	if rows == nil {
		t.Fatal("no rows")
	}
	if styles[StyleLineNum] == 0 {
		t.Fatalf("line-number style never painted")
	}
	if styles[StyleGlyphAdd] == 0 || styles[StyleGlyphDel] == 0 {
		t.Fatalf("glyph styles never painted: %+v", styles)
	}
}

func TestDiffGutterWidth(t *testing.T) {
	var old, cur strings.Builder
	for i := 1; i <= 1200; i++ {
		if i == 900 {
			old.WriteString("x\n")
			cur.WriteString("y\n")
			continue
		}
		old.WriteString("x\n")
		cur.WriteString("x\n")
	}
	d := Text("f", old.String(), cur.String(), Options{})
	if d.MaxNumber() != 3 {
		t.Fatalf("expected gutter width 3, got %d", d.MaxNumber())
	}
	rows := d.Render(nil)
	for _, r := range rows {
		if strings.HasPrefix(r.Raw, "900") && strings.Contains(r.Raw, "- x") {
			return
		}
	}
	t.Fatalf("expected deleted row gutter ' 900-':\n%s", truncate(d.Plain(), 400))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func TestDiffBlockFallbackHugeChange(t *testing.T) {
	var old, cur strings.Builder
	for range 400 {
		old.WriteString("old line with unique token\n")
		cur.WriteString("completely different content here\n")
	}
	d := Text("f", old.String(), cur.String(), Options{MaxLines: 5})
	if d.Added != 400 || d.Removed != 400 {
		t.Fatalf("counts should reflect full content, got +%d −%d", d.Added, d.Removed)
	}
	if d.TotalLines() > 5 || !d.Truncated {
		t.Fatalf("cap should truncate render")
	}
}

func TestDiffStatSummary(t *testing.T) {
	d := &File{Added: 5, Removed: 5}
	if got := d.StatSummary(); got != "+5 / -5 lines" {
		t.Errorf("StatSummary(5, 5) = %q, want '+5 / -5 lines'", got)
	}
	d2 := &File{Added: 3, Removed: 0}
	if got := d2.StatSummary(); got != "+3 lines" {
		t.Errorf("StatSummary(3, 0) = %q, want '+3 lines'", got)
	}
	d3 := &File{Added: 0, Removed: 2}
	if got := d3.StatSummary(); got != "-2 lines" {
		t.Errorf("StatSummary(0, 2) = %q, want '-2 lines'", got)
	}
}
