package tests

import (
	"strings"
	"testing"

	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
)

func TestParseMarkdownTable(t *testing.T) {
	lines := []string{
		"| Feature | Description | Status |",
		"| :--- | :---: | ---: |",
		"| Retries | Intelligent hints | 100% |",
		"| Tables | Box drawing | Active |",
	}

	tbl, ok := terminal.ParseMarkdownTable(lines)
	if !ok {
		t.Fatal("expected ParseMarkdownTable to succeed")
	}

	if len(tbl.Headers) != 3 {
		t.Fatalf("expected 3 headers, got %d", len(tbl.Headers))
	}
	if tbl.Headers[0] != "Feature" || tbl.Headers[1] != "Description" || tbl.Headers[2] != "Status" {
		t.Fatalf("unexpected headers: %v", tbl.Headers)
	}
	if tbl.Align[0] != terminal.AlignLeft || tbl.Align[1] != terminal.AlignCenter || tbl.Align[2] != terminal.AlignRight {
		t.Fatalf("unexpected alignments: %v", tbl.Align)
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(tbl.Rows))
	}
	if tbl.Rows[0][0] != "Retries" || tbl.Rows[1][2] != "Active" {
		t.Fatalf("unexpected row contents: %v", tbl.Rows)
	}
}

func TestRenderTableUnicode(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	tbl := &terminal.Table{
		Headers: []string{"Tool", "Speed"},
		Align:   []terminal.Alignment{terminal.AlignLeft, terminal.AlignRight},
		Rows: [][]string{
			{"read_file", "fast"},
			{"web_search", "net"},
		},
	}

	rendered := tbl.Render()
	if !strings.Contains(rendered, "┌") || !strings.Contains(rendered, "┐") || !strings.Contains(rendered, "┘") {
		t.Fatalf("expected Unicode box characters in table, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "read_file") || !strings.Contains(rendered, "fast") {
		t.Fatalf("missing table content in rendered output:\n%s", rendered)
	}
}

func TestRenderTableASCIIWhenThemeDisabled(t *testing.T) {
	theme.SetEnabled(false)
	defer theme.SetEnabled(false)

	tbl := &terminal.Table{
		Headers: []string{"A", "B"},
		Align:   []terminal.Alignment{terminal.AlignLeft, terminal.AlignLeft},
		Rows: [][]string{
			{"1", "2"},
		},
	}

	rendered := tbl.Render()
	if strings.Contains(rendered, "┌") {
		t.Fatalf("did not expect Unicode box characters when theme disabled, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "+") || !strings.Contains(rendered, "|") {
		t.Fatalf("expected ASCII box characters, got:\n%s", rendered)
	}
}

func TestTableStreamer(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	var output strings.Builder
	streamer := terminal.NewTableStreamer(func(s string) {
		output.WriteString(s)
	})

	chunks := []string{
		"Here is a summary:\n\n",
		"| Item | Count |\n",
		"| :--- | ---: |\n",
		"| Tests | 42 |\n",
		"| Tools | 7 |\n\n",
		"Done with summary.\n",
	}

	for _, c := range chunks {
		streamer.Write(c)
	}
	streamer.Flush()

	res := output.String()
	if !strings.Contains(res, "Here is a summary:") {
		t.Fatalf("missing prefix text, got:\n%s", res)
	}
	if !strings.Contains(res, "┌") || !strings.Contains(res, "Tests") {
		t.Fatalf("expected rendered box table, got:\n%s", res)
	}
	if !strings.Contains(res, "Done with summary.") {
		t.Fatalf("missing suffix text, got:\n%s", res)
	}
}

func TestVisibleWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"hello", 5},
		{"\x1b[31mred\x1b[0m", 3},
		{"⚡", 2},
		{"日本語", 6},
		{"✔ OK", 4},
		{"✗ FAIL", 6},
		{"⚠ TIMEOUT", 9},
	}
	for _, c := range cases {
		if got := terminal.VisibleWidth(c.in); got != c.want {
			t.Errorf("VisibleWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestThreeAndMoreColumnTableWrapping(t *testing.T) {
	theme.SetEnabled(true)
	defer theme.SetEnabled(false)

	lines := []string{
		"| Aspect | Go | Rust |",
		"| :--- | :--- | :--- |",
		"| **Memory Management** | Garbage collector (automatic, runtime overhead) | Ownership & borrowing system (no GC, compile-time checks) |",
		"| **Learning Curve** | Very simple; minimal syntax, learnable in days | Steep; lifetimes, traits, and ownership take months to master |",
		"| **Primary Use Cases** | Network services, cloud tools, CLIs (Docker, Kubernetes) | Systems programming, performance-critical apps (Firefox, Linux kernel modules) |",
	}

	tbl, ok := terminal.ParseMarkdownTable(lines)
	if !ok {
		t.Fatal("expected ParseMarkdownTable to succeed")
	}

	rendered := tbl.Render()

	// 1. Raw markdown asterisks must be converted to styling, not rendered as raw "**"
	if strings.Contains(rendered, "**Memory") || strings.Contains(rendered, "**") {
		t.Fatalf("rendered table should not contain raw asterisks, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Memory") || !strings.Contains(rendered, "Management") {
		t.Fatalf("missing 'Memory' or 'Management' in rendered output:\n%s", rendered)
	}

	// 2. Multi-line rows must have horizontal dividers between data rows
	if !strings.Contains(rendered, "├") || !strings.Contains(rendered, "┼") || !strings.Contains(rendered, "┤") {
		t.Fatalf("expected multi-line table to have row dividers, got:\n%s", rendered)
	}

	// 3. Check that lines wrap within bounds and every row has valid borders
	tableRows := strings.Split(strings.TrimSpace(rendered), "\n")
	for idx, row := range tableRows {
		width := terminal.VisibleWidth(row)
		if width > 85 {
			t.Errorf("row %d exceeds maximum table width: width=%d\n%s", idx, width, row)
		}
	}
}

func TestTableStreamerAndEdgeCases(t *testing.T) {
	var out strings.Builder
	streamer := terminal.NewTableStreamer(func(chunk string) {
		out.WriteString(chunk)
	})

	// 1. Regular text streamed
	streamer.Write("Normal line before table\n")

	// 2. Stream table rows chunk by chunk
	streamer.Write("| Col A | Col B |\n")
	streamer.Write("| --- | --- |\n")
	streamer.Write("| cell 1<br/>break | cell 2 with a verylongwordthatwillbebrokenacrosslinesbywrapcell |\n")
	streamer.Write("Normal line after table\n")
	streamer.Flush()

	res := out.String()
	if !strings.Contains(res, "Normal line before table") || !strings.Contains(res, "Normal line after table") {
		t.Fatalf("expected surrounding text in output, got:\n%s", res)
	}
	if !strings.Contains(res, "Col A") || !strings.Contains(res, "cell 1") {
		t.Fatalf("expected table content in output, got:\n%s", res)
	}

	// 3. Incomplete / invalid table rows streamed
	out.Reset()
	streamer = terminal.NewTableStreamer(func(chunk string) {
		out.WriteString(chunk)
	})
	streamer.Write("| Incomplete table without closing\n")
	streamer.Write("regular line\n")
	streamer.Flush()
	if !strings.Contains(out.String(), "Incomplete table") {
		t.Fatalf("expected unclosed table line in output, got:\n%s", out.String())
	}

	// 4. Pending table at Flush
	out.Reset()
	streamer = terminal.NewTableStreamer(func(chunk string) {
		out.WriteString(chunk)
	})
	streamer.Write("| H1 | H2 |\n| --- | --- |\n| Val1 | Val2 |")
	streamer.Flush()
	if !strings.Contains(out.String(), "Val1") {
		t.Fatalf("expected flushed table at EOF, got:\n%s", out.String())
	}
}

// A separator line that is just "|" must be rejected, not panic on slicing.
func TestParseMarkdownTableLonePipeSeparator(t *testing.T) {
	for _, sep := range []string{"|", "  |  "} {
		if _, ok := terminal.ParseMarkdownTable([]string{"| a | b |", sep, "| 1 | 2 |"}); ok {
			t.Errorf("separator %q: expected ParseMarkdownTable to reject the table", sep)
		}
	}
}
