package tests

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/diff"
	"hmj-agent/internal/session"
	"hmj-agent/internal/tools"
)

// --- Diff Benchmarks ---

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

// BenchmarkListFiles_FlatDirectory tests listing 200 files in a single flat directory.
func BenchmarkListFiles_FlatDirectory(b *testing.B) {
	tmpDir := b.TempDir()
	for i := range 200 {
		fPath := filepath.Join(tmpDir, fmt.Sprintf("file_%03d.go", i))
		if err := os.WriteFile(fPath, []byte("package main\n"), 0644); err != nil {
			b.Fatal(err)
		}
	}

	b.Chdir(tmpDir)
	tool := &tools.ListFilesTool{}
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		out, err := tool.Execute(ctx, `{"path":".","recursive":false}`)
		if err != nil || out == "" {
			b.Fatalf("list_files failed: %v", err)
		}
	}
}

// BenchmarkListFiles_DeepTree tests traversing a nested hierarchy up to max depth.
func BenchmarkListFiles_DeepTree(b *testing.B) {
	tmpDir := b.TempDir()

	// Create 4-level deep directory structure with files at each level
	for d1 := range 3 {
		p1 := filepath.Join(tmpDir, fmt.Sprintf("dir_%d", d1))
		for d2 := range 3 {
			p2 := filepath.Join(p1, fmt.Sprintf("sub_%d", d2))
			for d3 := range 3 {
				p3 := filepath.Join(p2, fmt.Sprintf("pkg_%d", d3))
				if err := os.MkdirAll(p3, 0755); err != nil {
					b.Fatal(err)
				}
				for f := range 3 {
					fPath := filepath.Join(p3, fmt.Sprintf("code_%d.go", f))
					if err := os.WriteFile(fPath, []byte("// code"), 0644); err != nil {
						b.Fatal(err)
					}
				}
			}
		}
	}

	b.Chdir(tmpDir)
	tool := &tools.ListFilesTool{}
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		out, err := tool.Execute(ctx, `{"path":".","recursive":true,"max_depth":4}`)
		if err != nil || out == "" {
			b.Fatalf("list_files failed: %v", err)
		}
	}
}

// BenchmarkListFiles_IgnoredPruning verifies that junk folders (.git, node_modules, vendor)
// containing thousands of mock entries are pruned immediately without descending.
func BenchmarkListFiles_IgnoredPruning(b *testing.B) {
	tmpDir := b.TempDir()

	// Create real source files
	for i := range 20 {
		if err := os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("main_%d.go", i)), []byte("package main"), 0644); err != nil {
			b.Fatal(err)
		}
	}

	// Create heavy ignored directories
	heavyDirs := []string{".git/objects/pack", "node_modules/@types/react", "vendor/github.com/heavy/pkg"}
	for _, dir := range heavyDirs {
		fullPath := filepath.Join(tmpDir, dir)
		if err := os.MkdirAll(fullPath, 0755); err != nil {
			b.Fatal(err)
		}
		for i := range 50 {
			fPath := filepath.Join(fullPath, fmt.Sprintf("dummy_%d.dat", i))
			if err := os.WriteFile(fPath, []byte("junk"), 0644); err != nil {
				b.Fatal(err)
			}
		}
	}

	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("node_modules/\nvendor/\n"), 0644); err != nil {
		b.Fatal(err)
	}

	b.Chdir(tmpDir)
	tool := &tools.ListFilesTool{}
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		out, err := tool.Execute(ctx, `{"path":".","recursive":true,"max_depth":4}`)
		if err != nil || out == "" {
			b.Fatalf("list_files failed: %v", err)
		}
	}
}

// --- Session Compaction & Serialization Benchmarks ---

func buildBenchmarkConversation(turnCount int, toolOutputLines int) *session.Conversation {
	conv := &session.Conversation{
		ID:   "bench-conversation",
		Name: "bench-session",
	}

	var toolBody strings.Builder
	for i := 1; i <= toolOutputLines; i++ {
		fmt.Fprintf(&toolBody, "output line %d: build succeeded with 0 errors\n", i)
	}
	bulkyOutput := toolBody.String()

	for turn := 1; turn <= turnCount; turn++ {
		conv.Messages = append(conv.Messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("Turn %d: please run check and update module", turn)},
			chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: fmt.Sprintf("call_%d", turn)}}},
			chat.Message{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", turn), Content: bulkyOutput},
			chat.Message{Role: "assistant", Content: fmt.Sprintf("Turn %d complete", turn)},
		)
	}

	return conv
}

// BenchmarkSessionCompact_10Turns tests compacting a 10-turn conversation with bulky tool outputs.
func BenchmarkSessionCompact_10Turns(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		b.StopTimer()
		conv := buildBenchmarkConversation(10, 50)
		b.StartTimer()

		pruned, saved := conv.Compact()
		if pruned == 0 || saved == 0 {
			b.Fatalf("expected compaction to prune messages")
		}
	}
}

// BenchmarkSessionCompact_50Turns tests Tier 1 + Tier 2 compaction on a deep 50-turn conversation.
func BenchmarkSessionCompact_50Turns(b *testing.B) {
	b.ReportAllocs()

	for b.Loop() {
		b.StopTimer()
		conv := buildBenchmarkConversation(50, 40)
		b.StartTimer()

		pruned, saved := conv.Compact()
		if pruned == 0 || saved == 0 {
			b.Fatalf("expected compaction to prune messages")
		}
	}
}

// BenchmarkSession_JSONSerialization measures session encoding overhead on long sessions.
func BenchmarkSession_JSONSerialization(b *testing.B) {
	conv := buildBenchmarkConversation(30, 30)
	conv.Compact()

	b.ResetTimer()
	b.ReportAllocs()

	for b.Loop() {
		data, err := json.Marshal(conv)
		if err != nil || len(data) == 0 {
			b.Fatalf("serialization failed: %v", err)
		}
	}
}

// --- Parsing & Matching Benchmarks ---

// buildBenchmarkHTML returns an HTML page of roughly size bytes mixing the markup that
// htmlToText has to handle: headings, paragraphs, links, lists, code, scripts and styles.
func buildBenchmarkHTML(size int) string {
	var sb strings.Builder
	sb.WriteString("<!doctype html><html><head><title>Bench</title><style>body{margin:0}.a{color:red}</style>" +
		"<script>var x = {\"a\": [1,2,3]}; function f(){ return x }</script></head><body>")
	for i := 0; sb.Len() < size; i++ {
		fmt.Fprintf(&sb, "<h2>Section %d</h2><p>Paragraph <b>%d</b> with a <a href=\"https://example.com/%d\">link</a> &amp; entity &lt;tag&gt;.</p>"+
			"<ul><li>first %d</li><li>second</li></ul><pre><code>func f%d() { return %d }</code></pre>"+
			"<!-- comment %d --><script>track(%d)</script><br><hr>\n", i, i, i, i, i, i, i, i)
	}
	sb.WriteString("</body></html>")
	return sb.String()
}

// BenchmarkFetchURL_HTMLToText200KB measures a loopback fetch of a ~200KB HTML page, which
// is dominated by the HTML-to-text conversion.
func BenchmarkFetchURL_HTMLToText200KB(b *testing.B) {
	page := buildBenchmarkHTML(200 * 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	}))
	defer server.Close()
	args := fmt.Sprintf(`{"url":%q,"max_lines":100000}`, server.URL)
	tool := &tools.FetchURLTool{}
	b.SetBytes(int64(len(page)))
	b.ReportAllocs()

	for b.Loop() {
		out, err := tool.Execute(context.Background(), args)
		if err != nil || !strings.Contains(out, "Section 0") || strings.Contains(out, "track(") {
			b.Fatalf("unexpected fetch result: err=%v len=%d", err, len(out))
		}
	}
}

// BenchmarkEditFile_FlexibleMatch5kLines measures the whitespace-insensitive match of a
// three-line block near the end of a 5,000-line file. The replacement restores the
// original text, so every iteration edits the same content.
func BenchmarkEditFile_FlexibleMatch5kLines(b *testing.B) {
	dir := b.TempDir()
	b.Chdir(dir)
	var content strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&content, "\tstep%d := compute(%d)\n", i, i)
	}
	if err := os.WriteFile("big.go", []byte(content.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	args, err := json.Marshal(map[string]any{
		"path":              "big.go",
		"old_string":        "    step4997 := compute(4997)\n      step4998 := compute(4998)\n    step4999 := compute(4999)",
		"new_string":        "\tstep4997 := compute(4997)\n\tstep4998 := compute(4998)\n\tstep4999 := compute(4999)",
		"ignore_whitespace": true,
	})
	if err != nil {
		b.Fatal(err)
	}
	tool := &tools.EditFileTool{}
	b.ReportAllocs()

	for b.Loop() {
		if _, err := tool.Execute(context.Background(), string(args)); err != nil {
			b.Fatalf("edit failed: %v", err)
		}
	}
}

// BenchmarkReadSSE_10kEvents measures parsing a stream of 10,000 events.
func BenchmarkReadSSE_10kEvents(b *testing.B) {
	var stream strings.Builder
	for i := range 10000 {
		fmt.Fprintf(&stream, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"token %d\"}\n\n", i)
	}
	data := stream.String()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()

	for b.Loop() {
		count := 0
		err := chat.ReadSSE(strings.NewReader(data), func(eventType, payload string) error {
			count++
			return nil
		})
		if err != nil || count != 10000 {
			b.Fatalf("ReadSSE: err=%v events=%d", err, count)
		}
	}
}
