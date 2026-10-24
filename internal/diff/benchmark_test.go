package diff_test

import (
	"fmt"
	"strings"
	"testing"

	"hmj-agent/internal/diff"
)

func generateLines(count int, prefix string) string {
	var sb strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&sb, "%s line %d: let val%d = %d * 2;\n", prefix, i, i, i)
	}
	return sb.String()
}

// BenchmarkDiff_SmallSurgicalEdit tests diffing a surgical edit in a 500-line file.
func BenchmarkDiff_SmallSurgicalEdit(b *testing.B) {
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("function taskStep%d() { return %d; }", i, i)
	}
	oldContent := strings.Join(lines, "\n") + "\n"

	// Surgical edit: modify 2 lines in the middle
	modified := make([]string, len(lines))
	copy(modified, lines)
	modified[250] = "function taskStep250() { return 99999; } // modified"
	modified[251] = "function taskStep251() { return 88888; } // modified"
	newContent := strings.Join(modified, "\n") + "\n"

	opts := diff.Options{Context: 3, Inline: true}
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		res := diff.Text("worker.js", oldContent, newContent, opts)
		if res.Added == 0 {
			b.Fatal("unexpected empty diff")
		}
	}
}

// BenchmarkDiff_MediumFile tests diffing a 2,000-line file with multiple scattered hunks.
func BenchmarkDiff_MediumFile(b *testing.B) {
	lines := make([]string, 2000)
	for i := range lines {
		lines[i] = fmt.Sprintf("const item_%d = { id: %d, active: true };", i, i)
	}
	oldContent := strings.Join(lines, "\n") + "\n"

	modified := make([]string, len(lines))
	copy(modified, lines)
	for idx := 100; idx < 1900; idx += 200 {
		modified[idx] = fmt.Sprintf("const item_%d = { id: %d, active: false, updated: true };", idx, idx)
	}
	newContent := strings.Join(modified, "\n") + "\n"

	opts := diff.Options{Context: 3, Inline: true}
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		res := diff.Text("data.ts", oldContent, newContent, opts)
		if res.Added == 0 {
			b.Fatal("unexpected empty diff")
		}
	}
}

// BenchmarkDiff_LargeFile_10kLines tests surgical edits in a 10,000-line file.
// Tests the efficiency of common prefix/suffix trimming.
func BenchmarkDiff_LargeFile_10kLines(b *testing.B) {
	lines := make([]string, 10000)
	for i := range lines {
		lines[i] = fmt.Sprintf("record_%05d: value_%05d", i, i)
	}
	oldContent := strings.Join(lines, "\n") + "\n"

	modified := make([]string, len(lines))
	copy(modified, lines)
	// Change 5 lines around line 5000
	for i := 5000; i < 5005; i++ {
		modified[i] = fmt.Sprintf("record_%05d: UPDATED_VALUE_%05d", i, i)
	}
	newContent := strings.Join(modified, "\n") + "\n"

	opts := diff.Options{Context: 3, Inline: true}
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		res := diff.Text("records.txt", oldContent, newContent, opts)
		if res.Added == 0 {
			b.Fatal("unexpected empty diff")
		}
	}
}

// BenchmarkDiff_LCSBudgetFallback tests large differing blocks where len(a)*len(b) > 90,000.
// Verifies that fallback to blockLines prevents quadratic time explosion.
func BenchmarkDiff_LCSBudgetFallback(b *testing.B) {
	oldContent := generateLines(500, "old_version")
	newContent := generateLines(500, "completely_new_version")

	opts := diff.Options{Context: 3, Inline: false}
	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		res := diff.Text("replace_all.txt", oldContent, newContent, opts)
		if res.Added == 0 {
			b.Fatal("unexpected empty diff")
		}
	}
}

// --- ListFiles Benchmarks ---
