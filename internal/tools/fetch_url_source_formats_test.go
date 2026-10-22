package tools

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// typedServer answers every request with body and the given Content-Type.
func typedServer(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const javaSource = "package android.app;\n\npublic class Notification {\n    static final int MAX_CHARSEQUENCE_LENGTH = 1024;\n}\n"

// A file gitiles serves as base64 for ?format=TEXT comes back as its source.
func TestFetchURLDecodesBase64FormatText(t *testing.T) {
	srv := textServer(t, base64.StdEncoding.EncodeToString([]byte(javaSource)))

	out := fetch(t, `{"url":"`+srv.URL+`/Notification.java?format=TEXT"}`)
	if out != "[decoded from base64]\n\n"+strings.TrimSpace(javaSource) {
		t.Errorf("got %q", out)
	}
}

// A long single base64 line served as text/plain is decoded without the format=TEXT query.
func TestFetchURLDecodesALongBase64Line(t *testing.T) {
	source := strings.Repeat(javaSource, 20)
	srv := textServer(t, base64.StdEncoding.EncodeToString([]byte(source))+"\n")

	out := fetch(t, `{"url":"`+srv.URL+`/raw","max_lines":1000}`)
	if out != "[decoded from base64]\n\n"+strings.TrimSpace(source) {
		t.Errorf("got %q", tail(out))
	}
}

// Text that only looks like base64, or encodes binary data, is left as it is.
func TestFetchURLKeepsTextThatIsNotBase64Text(t *testing.T) {
	binary := base64.StdEncoding.EncodeToString([]byte("PNG\x00\x01\x02 image bytes"))
	tests := []struct{ name, path, body string }{
		{name: "short token", path: "/token", body: "c2VjcmV0dG9rZW4xMjM0"},
		{name: "invalid base64 with format=TEXT", path: "/f?format=TEXT", body: "not base64 at all!"},
		{name: "binary file with format=TEXT", path: "/f?format=TEXT", body: binary},
		{name: "long line with spaces", path: "/prose", body: strings.Repeat("abcd ", 400)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := textServer(t, tc.body)
			if out := fetch(t, `{"url":"`+srv.URL+tc.path+`"}`); out != strings.TrimSpace(tc.body) {
				t.Errorf("got %q, want the body unchanged", out)
			}
		})
	}
}

// A minified JSON response is indented, so it has lines for max_lines to count.
func TestFetchURLIndentsJSON(t *testing.T) {
	srv := typedServer(t, "application/json; charset=utf-8", `{"name":"hmj","tags":["a","b"]}`)

	want := "{\n  \"name\": \"hmj\",\n  \"tags\": [\n    \"a\",\n    \"b\"\n  ]\n}"
	if out := fetch(t, `{"url":"`+srv.URL+`"}`); out != want {
		t.Errorf("got %q, want %q", out, want)
	}
	broken := typedServer(t, "application/json", `{"name":`)
	if out := fetch(t, `{"url":"`+broken.URL+`"}`); out != `{"name":` {
		t.Errorf("invalid JSON changed to %q", out)
	}
}

// fetch_url asks for Markdown first, and keeps a Markdown answer as written, even where it
// quotes HTML, instead of converting it as a page.
func TestFetchURLPrefersMarkdown(t *testing.T) {
	markdown := "# Guide\n\n```html\n<html><body>example</body></html>\n```\n\nSee [the API](/api.md)."
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		if strings.HasPrefix(accept, "text/markdown") {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Write([]byte(markdown))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><p>HTML version</p></body></html>"))
	}))
	defer srv.Close()

	out := fetch(t, `{"url":"`+srv.URL+`/guide"}`)
	if !strings.HasPrefix(accept, "text/markdown,text/html;q=0.9") {
		t.Errorf("Accept is %q, want text/markdown first", accept)
	}
	if out != "[the site served Markdown]\n\n"+markdown {
		t.Errorf("got %q", out)
	}
}

// Plain text that quotes HTML is not converted as a page.
func TestFetchURLKeepsPlainTextQuotingHTML(t *testing.T) {
	body := "Example template:\n<html>\n  <body>hi</body>\n</html>"
	srv := typedServer(t, "text/plain", body)
	if out := fetch(t, `{"url":"`+srv.URL+`"}`); out != body {
		t.Errorf("got %q", out)
	}
}

func TestRawGitHubURL(t *testing.T) {
	tests := map[string]string{
		"https://github.com/golang/go/blob/master/src/net/http/client.go":           "https://raw.githubusercontent.com/golang/go/master/src/net/http/client.go",
		"https://github.com/golang/go/blob/go1.27/README.md?plain=1#L10":            "https://raw.githubusercontent.com/golang/go/go1.27/README.md",
		"https://www.github.com/o/r/blob/abc123/dir/a%20b.txt":                      "https://raw.githubusercontent.com/o/r/abc123/dir/a%20b.txt",
		"https://github.com/golang/go":                                              "",
		"https://github.com/golang/go/tree/master/src":                              "",
		"https://github.com/golang/go/blob/master":                                  "",
		"https://gitlab.com/o/r/blob/main/file.go":                                  "",
		"https://raw.githubusercontent.com/golang/go/master/src/net/http/client.go": "",
	}
	for in, want := range tests {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := rawGitHubURL(u); got != want {
			t.Errorf("rawGitHubURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The tool description points the model at the conventions sites offer agents.
func TestFetchURLDescriptionNamesAgentConventions(t *testing.T) {
	desc := (&FetchURLTool{}).Definition().Function.Description
	for _, want := range []string{"/llms.txt", "/llms-full.txt", "plus .md", "Markdown first"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description does not mention %q", want)
		}
	}
}
