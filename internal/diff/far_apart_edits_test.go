package diff

import (
	"fmt"
	"strings"
	"testing"
)

// kotlinLike returns n lines shaped like source code: mostly unique lines, with the
// repeated braces and blank lines that make lines non-unique.
func kotlinLike(n int) []string {
	out := make([]string, n)
	for i := range out {
		switch i % 5 {
		case 3:
			out[i] = "    }"
		case 4:
			out[i] = ""
		default:
			out[i] = fmt.Sprintf("    val v%d = compute(%d)", i, i)
		}
	}
	return out
}

// TestFarApartEditsDiffOnlyWhatChanged is the multi-edit edit_file call on a long file: an
// import near the top and a line near the bottom change, 600 lines apart, a region past the
// LCS budget. Only those lines may show as changed, with their real numbers.
func TestFarApartEditsDiffOnlyWhatChanged(t *testing.T) {
	a := kotlinLike(700)
	b := append([]string(nil), a...)
	b[10] = "    val v10 = computeFaster(10)"
	b = append(b[:650], append([]string{"    // added", "    val extra = 1"}, b[650:]...)...)

	d := Text("f.kt", strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", Options{Inline: true, MaxLines: 120})
	if d.Added != 3 || d.Removed != 1 {
		t.Fatalf("got +%d / -%d, want +3 / -1", d.Added, d.Removed)
	}
	if d.Truncated {
		t.Fatalf("a 4-line change should not be cut: %d rows omitted", d.Omitted)
	}
	var changed []string
	for _, h := range d.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case Del:
				changed = append(changed, fmt.Sprintf("-%d %s", l.Old, l.Text))
			case Add:
				changed = append(changed, fmt.Sprintf("+%d %s", l.New, l.Text))
			}
		}
	}
	want := []string{
		"-11     val v10 = compute(10)",
		"+11     val v10 = computeFaster(10)",
		"+651     // added",
		"+652     val extra = 1",
	}
	if strings.Join(changed, "\n") != strings.Join(want, "\n") {
		t.Fatalf("changed rows:\n%s\nwant:\n%s", strings.Join(changed, "\n"), strings.Join(want, "\n"))
	}
	if len(d.Hunks) != 2 {
		t.Fatalf("got %d hunks, want 2", len(d.Hunks))
	}
}

// TestAnchorsKeepLineNumbersOnBothSides checks the numbers after anchors where the two sides
// have drifted apart: lines removed before an anchor shift every new number after it.
func TestAnchorsKeepLineNumbersOnBothSides(t *testing.T) {
	a := kotlinLike(800)
	b := append([]string(nil), a[:5]...)
	b = append(b, a[25:]...) // 20 lines removed near the top
	b[len(b)-3] = "    val changed = true"

	d := Text("f.kt", strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", Options{})
	if d.Removed != 21 || d.Added != 1 {
		t.Fatalf("got +%d / -%d, want +1 / -21", d.Added, d.Removed)
	}
	for _, h := range d.Hunks {
		for _, l := range h.Lines {
			if l.Kind == Ctx && a[l.Old-1] != b[l.New-1] {
				t.Fatalf("context row %d/%d pairs %q with %q", l.Old, l.New, a[l.Old-1], b[l.New-1])
			}
			if l.Kind == Add && b[l.New-1] != l.Text {
				t.Fatalf("added row %d is %q, file has %q", l.New, l.Text, b[l.New-1])
			}
			if l.Kind == Del && a[l.Old-1] != l.Text {
				t.Fatalf("removed row %d is %q, file had %q", l.Old, l.Text, a[l.Old-1])
			}
		}
	}
}

func TestPatienceAnchorsAreIncreasingOnBothSides(t *testing.T) {
	a := []string{"x", "a", "b", "dup", "c", "dup", "d"}
	b := []string{"c", "a", "dup", "b", "d", "y"}
	got := patienceAnchors(a, b)
	// "dup" is not unique in a; of a, b, c, d the longest increasing run is a, b, d.
	want := [][2]int{{1, 1}, {2, 3}, {6, 4}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
