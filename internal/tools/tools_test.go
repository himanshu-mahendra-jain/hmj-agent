package tools_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"hmj-agent/internal/tools"
)

func TestEditFileIgnoreWhitespace(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	tool := &tools.EditFileTool{}
	ctx := context.Background()

	filePath := "code.go"
	content := "\tfunc add(a, b int) int {\n\t\treturn a + b\n\t}\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Calling with spaces instead of tabs and ignore_whitespace: true succeeds
	req := `{"path":"code.go","old_string":"    func add(a, b int) int {\n        return a + b\n    }","new_string":"\tfunc add(a, b int) int {\n\t\treturn a + b + 1\n\t}","ignore_whitespace":true}`
	res, err := tool.Execute(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error with ignore_whitespace: %v", err)
	}
	if !strings.Contains(res, "successfully replaced 1 occurrence(s)") {
		t.Fatalf("unexpected result message: %s", res)
	}

	updated, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "return a + b + 1") {
		t.Fatalf("expected updated content, got: %s", string(updated))
	}
}

func TestEditFileAmbiguousIgnoreWhitespace(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	tool := &tools.EditFileTool{}
	ctx := context.Background()

	filePath := "duplicate.txt"
	content := "\tfunc foo() {\n\tfunc foo() {\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// old_string uses spaces, content uses tabs -> exact count is 0
	target := "    func foo() {\n"

	// Without replace_all, ambiguous ignore_whitespace should fail
	_, err := tool.Execute(ctx, `{"path":"duplicate.txt","old_string":"    func foo() {\n","new_string":"\tfunc bar() {\n","ignore_whitespace":true}`)
	if err == nil || !strings.Contains(err.Error(), "found 2 times with flexible whitespace") {
		t.Fatalf("expected ambiguous flexible error, got: %v", err)
	}

	// With replace_all: true, it should replace all occurrences
	res, err := tool.Execute(ctx, `{"path":"duplicate.txt","old_string":"`+strings.ReplaceAll(target, "\n", "\\n")+`","new_string":"\tfunc bar() {\n","ignore_whitespace":true,"replace_all":true}`)
	if err != nil {
		t.Fatalf("unexpected error with replace_all: %v", err)
	}
	if !strings.Contains(res, "successfully replaced 2 occurrence(s)") {
		t.Fatalf("unexpected response: %s", res)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "foo") {
		t.Fatalf("expected all foo to be replaced, got: %s", string(data))
	}
}

func TestEditFileOverlappingMatchesNoPanic(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	tool := &tools.EditFileTool{}
	ctx := context.Background()

	filePath := "overlap.txt"
	content := "\ta\n\ta\n\ta\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// old_string matches 2 consecutive lines. In a 3-line file, windows overlap if not advanced correctly.
	req := `{"path":"overlap.txt","old_string":"    a\n    a","new_string":"    b","ignore_whitespace":true,"replace_all":true}`
	res, err := tool.Execute(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error on overlapping replace: %v", err)
	}
	if !strings.Contains(res, "successfully replaced") {
		t.Fatalf("unexpected result: %s", res)
	}

	// The match window advance must apply the replacement exactly once
	// (first window covers lines 1-2; line 3 remains untouched).
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "    b\n\ta\n"; got != want {
		t.Fatalf("unexpected file content after overlapping replace: got %q, want %q", got, want)
	}
}

func TestEditFilePreviewRuneSafeUTF8(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	tool := &tools.EditFileTool{}
	ctx := context.Background()

	filePath := "cjk.txt"
	// 100 CJK characters (each 3 bytes in UTF-8, total 300 bytes > 80 rune threshold)
	cjkLine := strings.Repeat("日", 100)
	content := "\t" + cjkLine + "\n\t" + cjkLine + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Trigger whitespace near-match diagnostic with spaces
	req := `{"path":"cjk.txt","old_string":"    ` + cjkLine + `\n    ` + cjkLine + `","new_string":"// replaced"}`
	_, err := tool.Execute(ctx, req)
	if err == nil {
		t.Fatal("expected near match error")
	}
	errStr := err.Error()
	if !utf8.ValidString(errStr) {
		t.Fatalf("error message contains invalid UTF-8: %q", errStr)
	}
	if !strings.Contains(errStr, "...") {
		t.Fatalf("expected truncated preview with ellipsis, got: %s", errStr)
	}
}

func TestEditFileLineHintsAndFirstLineMatch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	tool := &tools.EditFileTool{}
	ctx := context.Background()

	filePath := "preview_test.go"
	content := "package main\n\nfunc compute() int {\n\tx := 10\n\treturn x\n}\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Whitespace mismatch shows line numbers and preview
	_, err := tool.Execute(ctx, `{"path":"preview_test.go","old_string":"  func compute() int {\n    x := 10\n    return x\n  }","new_string":"// replaced"}`)
	if err == nil {
		t.Fatal("expected error")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "near match exists with different whitespace or indentation at line(s) 3-6") {
		t.Errorf("expected line range 3-6 in error, got: %s", errStr)
	}
	if !strings.Contains(errStr, "line 3: func compute() int {") {
		t.Errorf("expected preview line 3 in error, got: %s", errStr)
	}

	// First line matches but subsequent lines differ
	_, err = tool.Execute(ctx, `{"path":"preview_test.go","old_string":"func compute() int {\n\tx := 999\n\treturn x\n}","new_string":"// replaced"}`)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "line 1 matches line 3, but subsequent lines differ") {
		t.Errorf("expected first line match diagnostic, got: %s", err.Error())
	}
}

func TestRunCommandHighFidelityDiagnostics(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	tool := &tools.RunCommandTool{}
	ctx := context.Background()

	// 1. Both stdout and stderr present
	res, err := tool.Execute(ctx, `{"command":"echo 'out msg'; >&2 echo 'err msg'; exit 7"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(res, "out msg\nerr msg\n") {
		t.Errorf("expected stdout then stderr in the order written, got: %s", res)
	}
	if !strings.Contains(res, "[Exit code 7]") {
		t.Errorf("expected formatted exit code notice, got: %s", res)
	}
	if strings.Contains(res, "exit status 7") {
		t.Errorf("exit notice must not repeat the redundant ExitError text, got: %s", res)
	}

	// 2. Only stderr present
	res, err = tool.Execute(ctx, `{"command":">&2 echo 'syntax error on line 5'; exit 1"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "syntax error on line 5") {
		t.Errorf("expected stderr message, got: %s", res)
	}
	if !strings.Contains(res, "[Exit code 1]") {
		t.Errorf("expected exit code 1, got: %s", res)
	}
}

func TestFastSyntaxCheckAutoFeedback(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	writeTool := &tools.WriteFileTool{}
	editTool := &tools.EditFileTool{}
	ctx := context.Background()

	// 1. Write invalid Go file -> syntax warning appended
	badGoPath := filepath.Join("pkg", "broken.go")
	res, err := writeTool.Execute(ctx, `{"path":"`+badGoPath+`","content":"package broken\nfunc foo( {\n"}`)
	if err != nil {
		t.Fatalf("write_file should not return fatal error on syntax issue: %v", err)
	}
	if !strings.Contains(res, "successfully wrote") {
		t.Fatalf("expected successful write: %s", res)
	}
	if !strings.Contains(res, "[syntax warning:") {
		t.Fatalf("expected syntax warning for broken Go code, got: %s", res)
	}

	// 2. Edit to valid Go file -> syntax warning gone
	res, err = editTool.Execute(ctx, `{"path":"`+badGoPath+`","old_string":"func foo( {\n","new_string":"func foo() {}\n"}`)
	if err != nil {
		t.Fatalf("unexpected edit error: %v", err)
	}
	if strings.Contains(res, "[syntax warning:") {
		t.Fatalf("expected no syntax warning for valid Go code, got: %s", res)
	}

	// 3. Write invalid JSON file -> syntax warning appended
	badJSONPath := "settings.json"
	res, err = writeTool.Execute(ctx, `{"path":"`+badJSONPath+`","content":"{\"key\": 123,}"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "[syntax warning: invalid JSON") {
		t.Fatalf("expected JSON syntax warning, got: %s", res)
	}

	// 4. Write valid JSON file -> no warning
	res, err = writeTool.Execute(ctx, `{"path":"`+badJSONPath+`","content":"{\"key\": 123}"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res, "[syntax warning:") {
		t.Fatalf("expected clean JSON write, got: %s", res)
	}

	// 5. Test FastSyntaxCheck helper directly on python
	pyCode := []byte("def test(:\n    pass\n")
	warn := tools.FastSyntaxCheck(ctx, "test.py", pyCode)
	if warn != "" && !strings.Contains(warn, "[syntax warning:") {
		t.Fatalf("expected syntax warning format, got: %s", warn)
	}
}

func TestFetchURLNegativeMaxLines(t *testing.T) {
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	}))
	defer server.Close()

	tool := &tools.FetchURLTool{}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fetch_url panicked on negative max_lines: %v", r)
		}
	}()

	res, err := tool.Execute(context.Background(), fmt.Sprintf(`{"url":%q,"max_lines":-10}`, server.URL))
	if err != nil {
		t.Fatalf("fetch_url failed: %v", err)
	}

	if !strings.Contains(res, "truncated to 120 lines") {
		t.Errorf("expected output to be clamped and truncated to 120 lines, got: %s", res)
	}
}

func TestWriteFile10MBCap(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	tool := &tools.WriteFileTool{}
	hugeContent := strings.Repeat("x", 11*1024*1024)
	_, err := tool.Execute(context.Background(), fmt.Sprintf(`{"path":"huge.txt","content":%q}`, hugeContent))
	if err == nil {
		t.Fatal("expected write_file to reject >10MB content")
	}
	if !strings.Contains(err.Error(), "too large (>10MB)") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestEditFile10MBCap(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	filePath := filepath.Join(dir, "large.txt")
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate to 11MB
	if err := f.Truncate(11 * 1024 * 1024); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	tool := &tools.EditFileTool{}
	_, err = tool.Execute(context.Background(), `{"path":"large.txt","old_string":"a","new_string":"b"}`)
	if err == nil {
		t.Fatal("expected edit_file to reject >10MB file")
	}
	if !strings.Contains(err.Error(), "too large (>10MB)") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// Commands inherit hmj's whole environment, credentials included: hmj runs on a dedicated VM.
func TestRunCommandInheritsEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-secret-12345")
	t.Setenv("GH_TOKEN", "ghp_secret_token_123")
	t.Setenv("NORMAL_USER_VAR", "preserved-val")

	res, err := (&tools.RunCommandTool{}).Execute(context.Background(), `{"command":"echo OAI=$OPENAI_API_KEY GH=$GH_TOKEN USR=$NORMAL_USER_VAR"}`)
	if err != nil {
		t.Fatalf("run_command failed: %v", err)
	}
	if want := "OAI=sk-secret-12345 GH=ghp_secret_token_123 USR=preserved-val"; !strings.Contains(res, want) {
		t.Errorf("output = %q, want %q", res, want)
	}
}

func TestComprehensiveWebSearchEndpointsAndDDG(t *testing.T) {
	// Mock DDG HTML endpoint returning search results
	htmlResp := `<html><body>
		<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Flearn&rut=test">Learn Go</a>
		<div class="result__snippet">Get started with Go programming language tutorials.</div>
	</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, htmlResp)
	}))
	defer srv.Close()

	tool := &tools.WebSearchTool{
		Client:          srv.Client(),
		DDGHTMLEndpoint: srv.URL,
	}

	res, err := tool.Execute(context.Background(), `{"query": "learn go", "limit": 3}`)
	if err != nil {
		t.Fatalf("unexpected search error: %v", err)
	}
	if !strings.Contains(res, "Learn Go") || !strings.Contains(res, "https://go.dev/learn") {
		t.Fatalf("expected decoded DDG link, got:\n%s", res)
	}
}

// TestParentContextCancellationSubprocess verifies that cancelling the parent
// context terminates long-running subprocesses and kills process groups cleanly.
func TestParentContextCancellationSubprocess(t *testing.T) {
	tool := &tools.RunCommandTool{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan string, 1)
	go func() {
		out, _ := tool.Execute(ctx, `{"command": "sleep 10"}`)
		done <- out
	}()

	// Allow process to spawn
	time.Sleep(50 * time.Millisecond)

	// Cancel the parent context
	cancel()

	select {
	case out := <-done:
		if !strings.Contains(out, "[command cancelled by user]") {
			t.Fatalf("expected cancellation notice in output, got: %q", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subprocess failed to terminate within 2s after parent context was cancelled")
	}

	// Verify no orphaned sleep 10 process is still lingering
	checkCmd := exec.Command("pgrep", "-f", "sleep 10")
	if out, err := checkCmd.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("orphaned sleep process found lingering: %s", string(out))
	}
}

func TestWebSearchContextCancellation(t *testing.T) {
	tool := &tools.WebSearchTool{
		DDGHTMLEndpoint:    "http://127.0.0.1:1/fail",
		BraveEndpoint:      "http://127.0.0.1:1/fail",
		BingEndpoint:       "http://127.0.0.1:1/fail",
		DDGInstantEndpoint: "http://127.0.0.1:1/fail",
		WikipediaEndpoint:  "http://127.0.0.1:1/fail",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Execute(ctx, `{"query": "golang"}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got: %v", err)
	}
}

func TestCappedWriterRunCommand(t *testing.T) {
	tool := &tools.RunCommandTool{}
	ctx := context.Background()

	// Generate ~100KB of output, which exceeds the 64KB buffer cap
	args, err := json.Marshal(map[string]any{
		"command": "head -c 100000 /dev/zero | tr '\\0' 'x'",
	})
	if err != nil {
		t.Fatalf("failed to marshal command args: %v", err)
	}

	out, err := tool.Execute(ctx, string(args))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if !strings.Contains(out, "omitted") {
		t.Fatalf("expected a note of the omitted output, got:\n%s", out)
	}
	if len(out) > 64*1024+512 {
		t.Fatalf("expected the output capped near 64KB plus the note, got %d bytes", len(out))
	}
}

func TestRunCommandUserCancellation(t *testing.T) {
	tool := &tools.RunCommandTool{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	args, err := json.Marshal(map[string]any{
		"command": "sleep 5",
	})
	if err != nil {
		t.Fatalf("failed to marshal command args: %v", err)
	}

	out, err := tool.Execute(ctx, string(args))
	if err != nil {
		t.Fatalf("expected nil error (reported in output), got %v", err)
	}
	if !strings.Contains(out, "[command cancelled by user]") {
		t.Fatalf("expected cancellation notice, got: %s", out)
	}
}

func TestFetchURLFetchesLoopbackServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("local server response"))
	}))
	defer ts.Close()

	// Dev servers started by the agent on localhost must be reachable.
	args, err := json.Marshal(map[string]any{"url": ts.URL})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}
	out, err := (&tools.FetchURLTool{}).Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("expected fetch of loopback server to succeed, got: %v", err)
	}
	if !strings.Contains(out, "local server response") {
		t.Fatalf("unexpected output: %s", out)
	}
}

// TestFetchURLRedirectToNonHTTPSchemeBlocked verifies the redirect policy.
func TestFetchURLRedirectToNonHTTPSchemeBlocked(t *testing.T) {
	var inner *httptest.Server
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/final", http.StatusFound)
	}))
	defer outer.Close()
	inner = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Final hop redirects to a file: URL which must be blocked.
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	defer inner.Close()

	tool := &tools.FetchURLTool{}
	_, err := tool.Execute(context.Background(), fmt.Sprintf(`{"url": "%s"}`, outer.URL))
	if err == nil {
		t.Fatal("expected redirect to file: scheme to be blocked")
	}
	if !strings.Contains(err.Error(), "blocked") && !strings.Contains(err.Error(), "redirect") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestFetchURLRedirectLoopCapped verifies redirect count capping.
func TestFetchURLRedirectLoopCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	tool := &tools.FetchURLTool{}
	_, err := tool.Execute(context.Background(), fmt.Sprintf(`{"url": "%s"}`, srv.URL))
	if err == nil {
		t.Fatal("expected redirect loop to be capped with an error")
	}
}

// TestRunCommandTimeoutDoesNotHang verifies a stubborn child process cannot
// wedge the tool past its timeout (WaitDelay bounds the pipe drain).
func TestRunCommandTimeoutDoesNotHang(t *testing.T) {
	t.Chdir(t.TempDir())
	tool := &tools.RunCommandTool{TimeoutDuration: 100 * time.Millisecond}

	res, err := tool.Execute(context.Background(), `{"command": "sleep 30 & sleep 30"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = res // completion within a bounded time is the assertion
}

// TestListFilesInvalidJSONArgs verifies malformed tool arguments surface an
// error instead of silently executing with zero values.
func TestListFilesInvalidJSONArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	tool := &tools.ListFilesTool{}

	_, err := tool.Execute(context.Background(), `{"path": "x", bad json`)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON arguments") {
		t.Errorf("expected invalid-JSON-arguments error, got: %v", err)
	}
}

// TestWebSearchDeterministicWithMock verifies the injectable endpoints keep
// web_search fully offline-deterministic for tests.
func TestWebSearchDeterministicWithMock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, map[string]any{
			"AbstractText": "Go is a compiled language.",
			"AbstractURL":  "https://go.dev",
		}); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer srv.Close()

	tool := &tools.WebSearchTool{
		Client:             srv.Client(),
		DDGHTMLEndpoint:    "http://127.0.0.1:1/unreachable", // force the page backends to fail fast
		BraveEndpoint:      "http://127.0.0.1:1/unreachable",
		BingEndpoint:       "http://127.0.0.1:1/unreachable",
		DDGInstantEndpoint: srv.URL + "/",
	}
	res, err := tool.Execute(context.Background(), `{"query": "go", "limit": 5}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "Go is a compiled language.") {
		t.Errorf("expected mock instant-answer result, got:\n%s", res)
	}
}

func TestWebSearchFallbacksAndValidation(t *testing.T) {
	// 1. Validation errors
	tool := &tools.WebSearchTool{}
	if _, err := tool.Execute(context.Background(), "{invalid"); err == nil {
		t.Fatal("expected error on invalid json")
	}
	if _, err := tool.Execute(context.Background(), `{"query": ""}`); err == nil {
		t.Fatal("expected error on empty query")
	}

	// 2. Wikipedia fallback
	wikiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Wikipedia article search format: {"query": {"search": [{title, snippet}]}}
		if err := json.MarshalWrite(w, map[string]any{"query": map[string]any{"search": []map[string]string{
			{"title": "Go (programming language)", "snippet": "Go is a statically typed compiled language."},
		}}}); err != nil {
			t.Errorf("failed to encode response: %v", err)
		}
	}))
	defer wikiSrv.Close()

	toolWithWiki := &tools.WebSearchTool{
		Client:             wikiSrv.Client(),
		DDGHTMLEndpoint:    "http://127.0.0.1:1/fail",
		BraveEndpoint:      "http://127.0.0.1:1/fail",
		BingEndpoint:       "http://127.0.0.1:1/fail",
		DDGInstantEndpoint: "http://127.0.0.1:1/fail",
		WikipediaEndpoint:  wikiSrv.URL + "/",
	}
	res, err := toolWithWiki.Execute(context.Background(), `{"query": "golang", "limit": 20}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "Go (programming language)") {
		t.Fatalf("expected wikipedia result, got:\n%s", res)
	}

	// 3. No search results found when all fail
	allFailTool := &tools.WebSearchTool{
		Client:             wikiSrv.Client(),
		DDGHTMLEndpoint:    "http://127.0.0.1:1/fail",
		BraveEndpoint:      "http://127.0.0.1:1/fail",
		BingEndpoint:       "http://127.0.0.1:1/fail",
		DDGInstantEndpoint: "http://127.0.0.1:1/fail",
		WikipediaEndpoint:  "http://127.0.0.1:1/fail",
	}
	res, err = allFailTool.Execute(context.Background(), `{"query": "unknownterm12345", "limit": 0}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "No search results found") {
		t.Fatalf("expected 'No search results found', got:\n%s", res)
	}
}

func TestReadFileTool(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	filePath := "test.txt"
	content := "line1\nline2\nline3\nline4\nline5"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &tools.ReadFileTool{}
	ctx := context.Background()

	// 1. Read entire file, without line numbers
	res, err := tool.Execute(ctx, `{"path":"`+filePath+`","numbered":false}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "line1\nline2\nline3\nline4\nline5") {
		t.Fatalf("unexpected content: %s", res)
	}

	// 2. Read with offset and limit
	res, err = tool.Execute(ctx, `{"path":"`+filePath+`","offset":2,"limit":2}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "2 | line2\n   3 | line3") {
		t.Fatalf("unexpected offset/limit slice: %s", res)
	}

	// 3. Read with numbered
	res, err = tool.Execute(ctx, `{"path":"`+filePath+`","offset":1,"limit":2,"numbered":true}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "1 | line1") || !strings.Contains(res, "2 | line2") {
		t.Fatalf("unexpected numbered output: %s", res)
	}

	// 4. Non-existent file
	_, err = tool.Execute(ctx, `{"path":"nonexistent.txt"}`)
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}

	// 5. Absolute paths outside the working directory are readable
	outsideFile := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside content"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err = tool.Execute(ctx, `{"path":"`+outsideFile+`"}`)
	if err != nil || res != "   1 | outside content" {
		t.Fatalf("expected outside file to be readable, got %q, %v", res, err)
	}

	// 6. Read large file (>500 lines) entirely without limits
	var largeSb strings.Builder
	for i := 1; i <= 800; i++ {
		largeSb.WriteString(fmt.Sprintf("echo line %d\n", i))
	}
	largePath := "large.sh"
	if err := os.WriteFile(largePath, []byte(largeSb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	res, err = tool.Execute(ctx, `{"path":"`+largePath+`"}`)
	if err != nil {
		t.Fatalf("unexpected error on large file: %v", err)
	}
	if !strings.Contains(res, "echo line 1\n") || !strings.Contains(res, "echo line 800") {
		t.Fatalf("expected full content of 800 lines, missing head/tail")
	}
	if strings.Contains(res, "showing lines") || strings.Contains(res, "truncated") {
		t.Fatalf("expected no truncation footer, got: %s", res)
	}

}

func TestWriteFileTool(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	targetPath := filepath.Join("nested", "sub", "hello.go")

	tool := &tools.WriteFileTool{}
	ctx := context.Background()

	res, err := tool.Execute(ctx, `{"path":"`+targetPath+`","content":"package hello\n"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "successfully wrote") {
		t.Fatalf("unexpected response: %s", res)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed reading written file: %v", err)
	}
	if string(data) != "package hello\n" {
		t.Fatalf("content mismatch: got %q", string(data))
	}

	// Absolute paths outside the working directory are writable, parents created
	outsidePath := filepath.Join(t.TempDir(), "deep", "outside.txt")
	res, err = tool.Execute(ctx, `{"path":"`+outsidePath+`","content":"outside"}`)
	if err != nil {
		t.Fatalf("unexpected error writing outside working directory: %v", err)
	}
	if !strings.Contains(res, "successfully wrote") {
		t.Fatalf("unexpected response: %s", res)
	}
	data, err = os.ReadFile(outsidePath)
	if err != nil || string(data) != "outside" {
		t.Fatalf("expected outside file to be written, got %q, %v", data, err)
	}
}

func TestEditFileTool(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	filePath := "edit_test.txt"
	initial := "foo bar foo baz"
	if err := os.WriteFile(filePath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &tools.EditFileTool{}
	ctx := context.Background()

	// 1. Ambiguous match without replace_all should fail
	_, err := tool.Execute(ctx, `{"path":"`+filePath+`","old_string":"foo","new_string":"qux"}`)
	if err == nil {
		t.Fatal("expected error for ambiguous match, got nil")
	}

	// 2. Surgical single match
	res, err := tool.Execute(ctx, `{"path":"`+filePath+`","old_string":"bar foo","new_string":"bar qux"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "successfully replaced") {
		t.Fatalf("unexpected output: %s", res)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "foo bar qux baz" {
		t.Fatalf("unexpected edited content: %q", string(data))
	}

	// 3. Replace all
	res, err = tool.Execute(ctx, `{"path":"`+filePath+`","old_string":"ba","new_string":"ZZ","replace_all":true}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "successfully replaced") {
		t.Fatalf("unexpected replace_all output: %s", res)
	}
	data, err = os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "foo ZZr qux ZZz" {
		t.Fatalf("unexpected replace_all content: %q", string(data))
	}

	// 4. Near match with different whitespace returns helpful diagnostic
	wsPath := "whitespace_test.txt"
	if err := os.WriteFile(wsPath, []byte("\tfunc main() {\n\t\tfmt.Println(\"hello\")\n\t}"), 0644); err != nil {
		t.Fatal(err)
	}
	// Agent queries with spaces instead of tabs
	_, err = tool.Execute(ctx, `{"path":"`+wsPath+`","old_string":"  func main() {\n    fmt.Println(\"hello\")\n  }","new_string":"// replaced"}`)
	if err == nil || !strings.Contains(err.Error(), "near match exists with different whitespace") {
		t.Fatalf("expected whitespace near-match diagnostic, got %v", err)
	}

	// 5. Files outside the working directory are editable by absolute path
	outsidePath := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("alpha beta"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, `{"path":"`+outsidePath+`","old_string":"alpha","new_string":"gamma"}`); err != nil {
		t.Fatalf("unexpected error editing outside file: %v", err)
	}
	data, err = os.ReadFile(outsidePath)
	if err != nil || string(data) != "gamma beta" {
		t.Fatalf("unexpected outside edit result: %q, %v", data, err)
	}
}

func TestListFilesTool(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	if err := os.MkdirAll("src", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".git", 0755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join("src", "main.go"):    "package main",
		filepath.Join("src", "app.min.js"): "console.log('min')",
		filepath.Join("src", ".DS_Store"):  "junk",
		filepath.Join(".git", "config"):    "git config",
	} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &tools.ListFilesTool{}
	ctx := context.Background()

	// 1. Without .gitignore: only .git is excluded, .DS_Store and app.min.js are NOT excluded
	res, err := tool.Execute(ctx, `{"path":"."}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(res, "src/main.go") {
		t.Fatalf("expected src/main.go in list, got: %s", res)
	}
	if strings.Contains(res, ".git") {
		t.Fatalf(".git should be ignored, got: %s", res)
	}
	if !strings.Contains(res, ".DS_Store") {
		t.Fatalf(".DS_Store should NOT be ignored without gitignore, got: %s", res)
	}
	if !strings.Contains(res, "app.min.js") {
		t.Fatalf("app.min.js should NOT be ignored without gitignore, got: %s", res)
	}

	// 2. With .gitignore: respects ignore rules and negation
	gitignoreContent := strings.Join([]string{
		"# Comment",
		".DS_Store",
		"*.min.js",
		"!keep.min.js",
		"ignored_folder/",
	}, "\n")
	if err := os.WriteFile(".gitignore", []byte(gitignoreContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("src", "keep.min.js"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("ignored_folder", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("ignored_folder", "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err = tool.Execute(ctx, `{"path":"."}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res, ".DS_Store") {
		t.Fatalf(".DS_Store should be ignored by .gitignore, got: %s", res)
	}
	if strings.Contains(res, "app.min.js") {
		t.Fatalf("app.min.js should be ignored by .gitignore, got: %s", res)
	}
	if !strings.Contains(res, "keep.min.js") {
		t.Fatalf("keep.min.js should be re-included via !negation, got: %s", res)
	}
	if strings.Contains(res, "ignored_folder") {
		t.Fatalf("ignored_folder should be ignored by .gitignore, got: %s", res)
	}

	// 3. Subdirectory .gitignore overrides
	if err := os.MkdirAll(filepath.Join("src", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("src", "sub", ".gitignore"), []byte("!sub.min.js\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("src", "sub", "sub.min.js"), []byte("sub min"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err = tool.Execute(ctx, `{"path":"src/sub"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "sub.min.js") {
		t.Fatalf("sub.min.js should be unignored by sub/.gitignore, got: %s", res)
	}

	// 4. Directories outside the working directory are listable, entries stay
	// relative to the requested directory, and its own .gitignore is honored.
	outsideDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outsideDir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join("pkg", "lib.go"):  "package pkg",
		filepath.Join("pkg", "run.log"): "log",
		".gitignore":                    "*.log\n",
	} {
		if err := os.WriteFile(filepath.Join(outsideDir, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	res, err = tool.Execute(ctx, `{"path":"`+outsideDir+`"}`)
	if err != nil {
		t.Fatalf("unexpected error listing outside directory: %v", err)
	}
	if !strings.Contains(res, "pkg/lib.go") {
		t.Fatalf("expected pkg/lib.go in outside listing, got: %s", res)
	}
	if strings.Contains(res, outsideDir) || strings.Contains(res, "run.log") {
		t.Fatalf("expected relative paths without ignored entries, got: %s", res)
	}
}

func TestRunCommandTool(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	tool := &tools.RunCommandTool{}
	ctx := context.Background()

	// 1. Basic command execution
	res, err := tool.Execute(ctx, `{"command":"echo hello world"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "hello world") {
		t.Fatalf("unexpected output: %s", res)
	}

	// 2. Chained pipeline and commands
	res, err = tool.Execute(ctx, `{"command":"echo apple | grep apple"}`)
	if err != nil {
		t.Fatalf("unexpected error with pipe: %v", err)
	}
	if !strings.Contains(res, "apple") {
		t.Fatalf("expected apple in output, got: %s", res)
	}

	// 3. Execution executes in working directory
	res, err = tool.Execute(ctx, `{"command":"pwd"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, tmpDir) {
		t.Fatalf("expected pwd to contain %s, got: %s", tmpDir, res)
	}

	// 3b. workdir runs the command in another directory
	otherDir := t.TempDir()
	res, err = tool.Execute(ctx, `{"command":"pwd","workdir":"`+otherDir+`"}`)
	if err != nil {
		t.Fatalf("unexpected error with workdir: %v", err)
	}
	if !strings.Contains(res, otherDir) {
		t.Fatalf("expected pwd to contain workdir %s, got: %s", otherDir, res)
	}

	// 3c. Invalid workdir (missing, or a regular file) is an error
	if _, err := tool.Execute(ctx, `{"command":"pwd","workdir":"`+filepath.Join(tmpDir, "missing")+`"}`); err == nil {
		t.Fatal("expected error for missing workdir")
	}
	filePath := filepath.Join(tmpDir, "plain.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, `{"command":"pwd","workdir":"`+filePath+`"}`); err == nil {
		t.Fatal("expected error when workdir is a file")
	}

	// 4. Command with exit code failure returns error in output
	res, err = tool.Execute(ctx, `{"command":"exit 42"}`)
	if err != nil {
		t.Fatalf("unexpected tool error on non-zero exit: %v", err)
	}
	if !strings.Contains(res, "[Exit code 42]") {
		t.Fatalf("expected exit code 42 notice, got: %s", res)
	}
	if strings.Contains(res, "exit status 42") {
		t.Fatalf("exit notice must not repeat the redundant ExitError text, got: %s", res)
	}
}

func TestResolvePath(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty is cwd", "", wd},
		{"dot is cwd", ".", wd},
		{"surrounding spaces trimmed", "  spaced.txt  ", filepath.Join(wd, "spaced.txt")},
		{"relative is under cwd", "sub/file.txt", filepath.Join(wd, "sub", "file.txt")},
		{"relative cleaned", "./a/../b.txt", filepath.Join(wd, "b.txt")},
		{"absolute kept", "/etc/hosts", "/etc/hosts"},
		{"parent traversal allowed", "../outside.txt", filepath.Join(filepath.Dir(wd), "outside.txt")},
		{"tilde is home", "~", home},
		{"tilde slash is home", "~/notes/x.md", filepath.Join(home, "notes", "x.md")},
		{"tilde user is not expanded", "~user/x", filepath.Join(wd, "~user", "x")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tools.ResolvePath(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.expected {
				t.Fatalf("ResolvePath(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestFetchURLTool(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><script>alert(1);</script><style>body{color:red;}</style></head><body><h1>Documentation</h1><p>Welcome to <b>HMJ</b>. See <a href="https://example.com/docs">docs link</a>.</p><ul><li>Feature A</li><li>Feature B</li></ul></body></html>`)
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00, 0x00})
		case "/long":
			w.Header().Set("Content-Type", "text/plain")
			for i := 1; i <= 30; i++ {
				fmt.Fprintf(w, "line %d\n", i)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	tool := &tools.FetchURLTool{Client: ts.Client()}
	ctx := context.Background()

	// 1. HTML conversion
	res, err := tool.Execute(ctx, fmt.Sprintf(`{"url": "%s/html"}`, ts.URL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res, "alert(1)") || strings.Contains(res, "color:red") {
		t.Errorf("expected scripts and styles to be stripped, got:\n%s", res)
	}
	if !strings.Contains(res, "Documentation") || !strings.Contains(res, "[docs link](https://example.com/docs)") {
		t.Errorf("expected converted markdown headings and links, got:\n%s", res)
	}

	// 2. Binary detection and helpful curl suggestion
	res, err = tool.Execute(ctx, fmt.Sprintf(`{"url": "%s/binary"}`, ts.URL))
	if err != nil {
		t.Fatalf("unexpected error on binary: %v", err)
	}
	if !strings.Contains(res, "binary content") || !strings.Contains(res, "curl -fsSL -o <filename>") {
		t.Errorf("expected binary notification with curl command, got:\n%s", res)
	}

	// 3. Max lines truncation
	res, err = tool.Execute(ctx, fmt.Sprintf(`{"url": "%s/long", "max_lines": 5}`, ts.URL))
	if err != nil {
		t.Fatalf("unexpected error on long text: %v", err)
	}
	if !strings.Contains(res, "truncated to 5 lines") {
		t.Errorf("expected truncation notice, got:\n%s", res)
	}
}

func TestWebSearchTool(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"query":{"search":[{"title":"Go (programming language)","snippet":"Go is an open source programming language."}]}}`)
	}))
	defer ts.Close()

	tool := &tools.WebSearchTool{
		Client:             ts.Client(),
		DDGHTMLEndpoint:    "http://127.0.0.1:1/unreachable", // force the other backends to fail so
		BraveEndpoint:      "http://127.0.0.1:1/unreachable", // the mock Wikipedia result is
		BingEndpoint:       "http://127.0.0.1:1/unreachable", // deterministic
		DDGInstantEndpoint: "http://127.0.0.1:1/unreachable",
		WikipediaEndpoint:  ts.URL + "/",
	}
	ctx := context.Background()

	res, err := tool.Execute(ctx, `{"query": "golang"}`)
	if err != nil {
		t.Fatalf("unexpected search error: %v", err)
	}
	if !strings.Contains(res, "Go (programming language)") || !strings.Contains(res, "https://en.wikipedia.org/wiki/Go_(programming_language)") {
		t.Errorf("expected search results to contain Go, got:\n%s", res)
	}
}

// fetch_url presents a generic browser User-Agent that does not name the agent.
func TestFetchURLUserAgentIsGeneric(t *testing.T) {
	var ua string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.UserAgent()
		fmt.Fprint(w, "ok")
	}))
	defer ts.Close()
	tool := &tools.FetchURLTool{Client: ts.Client()}
	if _, err := tool.Execute(context.Background(), fmt.Sprintf(`{"url": %q}`, ts.URL)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ua, "Mozilla/5.0") || strings.Contains(strings.ToLower(ua), "hmj") {
		t.Errorf("User-Agent = %q, want a generic browser string", ua)
	}
}
