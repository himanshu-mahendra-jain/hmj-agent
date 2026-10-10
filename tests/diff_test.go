package tests

import (
	"strings"
	"testing"

	"hmj-agent/internal/diff"
)

func TestComprehensiveDiffInlineAndCap(t *testing.T) {
	oldText := "apple\nbanana pie\ncherry\n"
	newText := "apple\nbanana split\ncherry\n"

	d := diff.Text("food.txt", oldText, newText, diff.Options{Inline: true, Context: 1, MaxLines: 5})
	if d.Empty() {
		t.Fatal("expected non-empty diff")
	}

	rows := d.Render(func(s string, style diff.SegmentStyle) string {
		return s
	})
	if len(rows) == 0 {
		t.Fatal("expected rendered rows")
	}

	plain := d.Plain()
	if !strings.Contains(plain, "food.txt") {
		t.Fatalf("plain output missing path: %s", plain)
	}
}
