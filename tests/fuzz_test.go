package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/diff"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/tools"
)

// FuzzResolvePath ensures arbitrary paths (including traversal, NUL bytes,
// unicode, "~" forms, and absolute paths) never panic and always resolve to a
// clean absolute path.
func FuzzResolvePath(f *testing.F) {
	f.Add("ok.txt")
	f.Add("sub/dir/file.go")
	f.Add("../secret")
	f.Add("../../etc/passwd")
	f.Add("/etc/shadow")
	f.Add("sub/../../escape")
	f.Add("file\x00name")
	f.Add("")
	f.Add(" ")
	f.Add("\n\r\t")
	f.Add("./../../")
	f.Add("....//....//....//etc")
	f.Add("~")
	f.Add("~/notes.txt")
	f.Add("~user/notes.txt")

	f.Fuzz(func(t *testing.T, userPath string) {
		got, err := tools.ResolvePath(userPath)
		if err != nil {
			return
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("expected absolute path, input=%q, resolved=%q", userPath, got)
		}
		if filepath.Clean(got) != got {
			t.Fatalf("expected cleaned path, input=%q, resolved=%q", userPath, got)
		}
	})
}

// FuzzDiffText ensures that diff calculations and hunk generation never panic
// on arbitrary content (multi-byte UTF-8, binary bytes, empty strings, mismatched lines).
func FuzzDiffText(f *testing.F) {
	f.Add("hello", "world")
	f.Add("line 1\nline 2\n", "line 1\nline 2 modified\n")
	f.Add("", "")
	f.Add("\x00\xff\xfe", "\xfe\xff\x00")
	f.Add("const a = 1\n", "const a = 2\n")
	f.Add("multi\nline\none\ntwo\nthree", "multi\nline\none\nmodified\nthree")

	f.Fuzz(func(t *testing.T, a, b string) {
		res := diff.Text("fuzz_target.txt", a, b, diff.Options{Context: 3, Inline: true})
		if res == nil {
			t.Fatal("expected non-nil diff.File")
		}
		_ = res.Empty()
		_ = res.TotalLines()
		_ = res.MaxNumber()
	})
}

// FuzzToolArguments verifies that arbitrary/malformed JSON strings generated
// by the model never cause tool executors to panic.
func FuzzToolArguments(f *testing.F) {
	f.Add("read_file", `{"path": "README.md"}`)
	f.Add("read_file", `{"path": "../../etc/passwd"}`)
	f.Add("read_file", `{"path": 12345}`)
	f.Add("read_file", `malformed json`)
	f.Add("edit_file", `{"path": "README.md", "old_string": "foo", "new_string": "bar"}`)
	f.Add("edit_file", `{"old_string": null}`)
	f.Add("list_files", `{}`)
	f.Add("list_files", `{"path": "../"}`)
	f.Add("fetch_url", `{"url": "http://127.0.0.1:8080"}`)
	f.Add("fetch_url", `{"url": "not-a-url"}`)
	f.Add("web_search", `{"query": "golang"}`)
	f.Add("web_search", `{"query": ""}`)

	fuzzableTools := map[string]tools.Tool{
		"read_file":  &tools.ReadFileTool{},
		"edit_file":  &tools.EditFileTool{},
		"list_files": &tools.ListFilesTool{},
		"fetch_url":  &tools.FetchURLTool{},
		"web_search": &tools.WebSearchTool{},
	}

	f.Fuzz(func(t *testing.T, toolName, argsJSON string) {
		tool, ok := fuzzableTools[toolName]
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		// Execute must never panic, regardless of input corruption
		_, _ = tool.Execute(ctx, argsJSON)
	})
}

// FuzzParseRateLimitHint checks the retry hint is never negative and never exceeds a day,
// whatever the provider's error body says.
func FuzzParseRateLimitHint(f *testing.F) {
	f.Add("")
	f.Add("Please try again in 4m39s")
	f.Add("You have exceeded your quota, retry in 1h2m3s")
	f.Add("retry in 90 seconds")
	f.Add("try again in 2 hours and 15 mins")
	f.Add("retry after 999999999h")
	f.Add("retry in 99999999999999999999s")
	f.Add("retry in 1.5.5s")
	f.Add("retry in 3 months")
	f.Add("RETRY IN 10 MS")
	f.Add("rate limit: try in 5, 6m")

	f.Fuzz(func(t *testing.T, body string) {
		d := chat.ParseRateLimitHint(body)
		if d < 0 || d > 24*time.Hour {
			t.Fatalf("ParseRateLimitHint(%q) = %v, want 0..24h", body, d)
		}
	})
}

// FuzzCompactToolOutput checks pruning of old tool output: it never grows a message, never
// touches other messages, keeps the head and tail of what it shortens, and is idempotent.
func FuzzCompactToolOutput(f *testing.F) {
	var long strings.Builder
	for i := range 60 {
		long.WriteString("output line " + strings.Repeat("x", i) + "\n")
	}
	f.Add(long.String())
	f.Add(strings.Repeat("a", 5000))
	f.Add(strings.Repeat("日本語", 800))
	f.Add("short\n")
	f.Add("")
	f.Add(strings.Repeat("\n", 100))
	f.Add(strings.Repeat("l\n", 30) + "pruned for context efficiency")
	f.Add(strings.Repeat("\xff\xfe", 700))

	f.Fuzz(func(t *testing.T, output string) {
		conv := &session.Conversation{Messages: []chat.Message{
			{Role: "user", Content: "first"},
			{Role: "tool", ToolCallID: "c1", Content: output},
			{Role: "assistant", Content: "done"},
			{Role: "user", Content: "second"},
		}}
		pruned, saved := conv.PruneOldToolOutputs(1)
		got := conv.Messages[1].Content
		if len(got) > len(output) {
			t.Fatalf("pruned output is longer: %d > %d", len(got), len(output))
		}
		if saved != len(output)-len(got) || (pruned == 1) != (got != output) {
			t.Fatalf("reported pruned=%d saved=%d, but len %d -> %d", pruned, saved, len(output), len(got))
		}
		if conv.Messages[0].Content != "first" || conv.Messages[2].Content != "done" || conv.Messages[3].Content != "second" {
			t.Fatal("non-tool messages were modified")
		}
		if got != output {
			checkPrunedKeepsEnds(t, output, got)
			if p, _ := conv.PruneOldToolOutputs(1); p != 0 || conv.Messages[1].Content != got {
				t.Fatal("pruning twice changed the output again")
			}
		}
		if utf8.ValidString(output) && !utf8.ValidString(got) {
			t.Fatalf("pruning cut a multi-byte character: %q", got)
		}
	})
}

func checkPrunedKeepsEnds(t *testing.T, output, got string) {
	t.Helper()
	lines := strings.Split(output, "\n")
	if len(lines) > session.RetainHeadLines+session.RetainTailLines {
		head := strings.Join(lines[:session.RetainHeadLines], "\n") + "\n"
		tail := strings.Join(lines[len(lines)-session.RetainTailLines:], "\n")
		if !strings.HasPrefix(got, head) || !strings.HasSuffix(got, tail) || !strings.Contains(got, "lines pruned") {
			t.Fatalf("head/tail lost: in=%q out=%q", output, got)
		}
		return
	}
	// Few very long lines: the first and last bytes survive (a split character may cost 3).
	if !strings.HasPrefix(got, output[:396]) || !strings.HasSuffix(got, output[len(output)-196:]) || !strings.Contains(got, "bytes pruned") {
		t.Fatalf("byte head/tail lost: in len %d, out %q", len(output), got)
	}
}

// FuzzUnquoteDragPath checks drag-and-drop path cleanup never grows the text, leaves plain
// paths alone and strips exactly one pair of matching quotes.
func FuzzUnquoteDragPath(f *testing.F) {
	f.Add("/tmp/file.txt")
	f.Add("'/tmp/my file.txt'")
	f.Add("\"/tmp/my file.txt\"")
	f.Add("/tmp/my\\ file.txt")
	f.Add("file:///tmp/a%20b.txt")
	f.Add("file://localhost/tmp/x")
	f.Add("file://host")
	f.Add("file:///tmp/%zz%4")
	f.Add("'it's'")
	f.Add("  /padded  \n")
	f.Add("''")
	f.Add("%")

	f.Fuzz(func(t *testing.T, in string) {
		out := terminal.UnquoteDragPath(in)
		if len(out) > len(in) {
			t.Fatalf("output longer than input: %q -> %q", in, out)
		}
		trimmed := strings.TrimSpace(in)
		if !strings.ContainsAny(trimmed, "'\"\\%") && !strings.HasPrefix(trimmed, "file://") && out != trimmed {
			t.Fatalf("plain path changed: %q -> %q", in, out)
		}
		if n := len(trimmed); n >= 2 && trimmed[0] == '\'' && trimmed[n-1] == '\'' && !strings.Contains(trimmed[1:n-1], "'") &&
			!strings.HasPrefix(trimmed, "file://") && out != trimmed[1:n-1] {
			t.Fatalf("single quotes not stripped: %q -> %q", in, out)
		}
	})
}

// A stray invalid byte in the head of a long single-line output (binary data, Latin-1
// text) must not discard the head: only a rune split by the cut is dropped.
func TestPruneKeepsHeadOfInvalidUTF8(t *testing.T) {
	output := "HEADMARK" + strings.Repeat("\xff", 3000)
	conv := &session.Conversation{Messages: []chat.Message{
		{Role: "user", Content: "a"}, {Role: "tool", Content: output}, {Role: "user", Content: "b"},
	}}
	conv.PruneOldToolOutputs(1)
	if !strings.Contains(conv.Messages[1].Content, "HEADMARK") {
		t.Errorf("head lost: %q", conv.Messages[1].Content[:40])
	}
}
