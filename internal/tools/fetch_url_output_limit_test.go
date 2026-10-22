package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// textServer answers every request with body as text/plain.
func textServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useFetchDir turns on saving of truncated pages to a temporary directory for one test.
func useFetchDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetFetchDir(dir)
	t.Cleanup(func() { SetFetchDir("") })
	return dir
}

var savedPathRE = regexp.MustCompile(`saved to (\S+): read it with read_file`)

// savedText returns the path the result names and the content of that file.
func savedText(t *testing.T, out string) (string, string) {
	t.Helper()
	m := savedPathRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no saved file named in %q", tail(out))
	}
	data, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return m[1], string(data)
}

func fetch(t *testing.T, args string) string {
	t.Helper()
	out, err := (&FetchURLTool{}).Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A response that is one long line, such as minified code, is cut to
// MaxFetchOutputBytes however few lines it has, and saved whole.
func TestFetchURLCutsOneLongLineAndSavesIt(t *testing.T) {
	dir := useFetchDir(t)
	body := strings.Repeat("var a=1;", 100*1024) // 800 KB of minified code, no newline
	srv := textServer(t, body)

	out := fetch(t, `{"url":"`+srv.URL+`/a/b.java"}`)
	if len(out) > MaxFetchOutputBytes+1024 {
		t.Errorf("result is %d bytes, want about %d", len(out), MaxFetchOutputBytes)
	}
	if !strings.Contains(out, "[truncated to 64 KB: line 1 of 1 is 800 KB long and was cut;") {
		t.Errorf("missing cut note in %q", tail(out))
	}
	path, saved := savedText(t, out)
	if saved != body {
		t.Errorf("saved %d bytes, want the whole %d-byte body", len(saved), len(body))
	}
	if filepath.Dir(path) != dir || !strings.HasSuffix(path, "_a_b.java.txt") {
		t.Errorf("saved at %s, want a file in %s", path, dir)
	}
}

// A page with more lines than max_lines shows the first ones and saves every line.
func TestFetchURLSavesAPageCutByLines(t *testing.T) {
	useFetchDir(t)
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	srv := textServer(t, b.String())

	out := fetch(t, `{"url":"`+srv.URL+`"}`)
	if !strings.Contains(out, "line 119\n") || strings.Contains(out, "line 120\n") {
		t.Errorf("want lines 0-119 only, got %q", tail(out))
	}
	if !strings.Contains(out, "[truncated to 120 lines of 500, 4 KB in all; the full text is saved to ") {
		t.Errorf("missing note in %q", tail(out))
	}
	if _, saved := savedText(t, out); saved != strings.TrimSpace(b.String()) {
		t.Errorf("saved text differs from the page")
	}
}

// A page within both limits comes back whole, with no note and no file.
func TestFetchURLLeavesASmallPageAlone(t *testing.T) {
	dir := useFetchDir(t)
	srv := textServer(t, "just a little text\n")

	if out := fetch(t, `{"url":"`+srv.URL+`"}`); out != "just a little text" {
		t.Errorf("got %q", out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("saved %d files for a page that was not cut", len(entries))
	}
}

// Without a fetch directory the note suggests max_lines, but only while it can show more.
func TestFetchURLNoteWithoutSaving(t *testing.T) {
	var b strings.Builder
	for i := range 1500 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	srv := textServer(t, b.String())

	if out := fetch(t, `{"url":"`+srv.URL+`","max_lines":10}`); !strings.HasSuffix(out, "[truncated to 10 lines of 1500, 13 KB in all; use max_lines to view more] ...") {
		t.Errorf("got %q", tail(out))
	}
	if out := fetch(t, `{"url":"`+srv.URL+`","max_lines":1000}`); strings.Contains(out, "use max_lines") {
		t.Errorf("suggests max_lines at its limit: %q", tail(out))
	}
}

// A failed save is reported in the note; the first page is still returned.
func TestFetchURLReportsAFailedSave(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	SetFetchDir(notADir)
	t.Cleanup(func() { SetFetchDir("") })
	srv := textServer(t, strings.Repeat("x\n", 200))

	out := fetch(t, `{"url":"`+srv.URL+`"}`)
	if !strings.HasPrefix(out, "x\n") || !strings.Contains(out, "; the full text could not be saved: ") {
		t.Errorf("got %q", tail(out))
	}
}

// The saved file's name keeps the host and path in safe characters, ending with the page name.
func TestFetchFileName(t *testing.T) {
	tests := map[string]string{
		"https://android.googlesource.com/platform/+/main/Notification.java?format=TEXT": "android.googlesource.com_platform___main_Notification.java",
		"https://example.com/":                                  "example.com",
		"http://127.0.0.1:8080/docs/a b":                        "127.0.0.1_8080_docs_a_b",
		"https://x.dev/" + strings.Repeat("d/", 150) + "end.md": strings.Repeat("d_", 150)[300-(maxBackupNameBytes-len("end.md")):] + "end.md",
	}
	for in, want := range tests {
		if got := fetchFileName(in); got != want {
			t.Errorf("fetchFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Saved pages are pruned to maxFetchFiles, oldest first.
func TestFetchDirIsPruned(t *testing.T) {
	dir := useFetchDir(t)
	for i := range maxFetchFiles + 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("20200101-000000.%06d_old.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv := textServer(t, strings.Repeat("y\n", 200))
	_ = fetch(t, `{"url":"`+srv.URL+`"}`)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != maxFetchFiles {
		t.Fatalf("%d files left, want %d", len(entries), maxFetchFiles)
	}
	if entries[0].Name() != "20200101-000000.000006_old.txt" {
		t.Errorf("oldest kept file is %s; the six oldest should be gone", entries[0].Name())
	}
}
