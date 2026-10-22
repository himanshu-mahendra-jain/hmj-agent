package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"net/url"
	"strings"
	"unicode/utf8"
)

// minBase64Bytes is the shortest text/plain body taken for base64 without a format=TEXT query.
// Shorter bodies, such as a token or an ID, are left as they are.
const minBase64Bytes = 1024

// rawGitHubURL returns the raw.githubusercontent.com URL of a GitHub file page
// (github.com/<owner>/<repo>/blob/<ref>/<path>), or "" for any other URL. The file page is
// HTML around the code; the raw URL is the file itself.
func rawGitHubURL(u *url.URL) string {
	if !strings.EqualFold(u.Host, "github.com") && !strings.EqualFold(u.Host, "www.github.com") {
		return ""
	}
	parts := strings.SplitN(strings.TrimPrefix(u.EscapedPath(), "/"), "/", 5)
	if len(parts) < 5 || parts[2] != "blob" || parts[4] == "" {
		return ""
	}
	return "https://raw.githubusercontent.com/" + parts[0] + "/" + parts[1] + "/" + parts[3] + "/" + parts[4]
}

// decodeBase64Text returns the text that body encodes in base64, as gitiles (googlesource.com)
// serves a file for ?format=TEXT. It decodes when the URL asks for that format, or when body is
// a text/plain line of at least minBase64Bytes; either way only when the result is valid UTF-8
// with no NUL byte, so binary files and text that merely looks like base64 stay as they are.
func decodeBase64Text(u *url.URL, contentType string, body []byte) (string, bool) {
	trimmed := bytes.TrimSpace(body)
	requested := u.Query().Get("format") == "TEXT"
	if !requested && (!strings.Contains(contentType, "text/plain") || len(trimmed) < minBase64Bytes || bytes.ContainsAny(trimmed, " \t\r\n")) {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(string(trimmed))
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) || bytes.IndexByte(decoded, 0) != -1 {
		return "", false
	}
	return string(decoded), true
}

// indentJSON returns body indented two spaces per level when it is a JSON response, so a
// minified document gets lines that max_lines can count.
func indentJSON(contentType string, body []byte) (string, bool) {
	if !strings.Contains(contentType, "json") {
		return "", false
	}
	v := jsontext.Value(bytes.Clone(body))
	if v.Indent(jsontext.WithIndent("  ")) != nil {
		return "", false
	}
	return string(v), true
}

// isHTMLResponse reports whether a response should be converted from HTML to text: its type
// says HTML, or it has no text type and its body looks like an HTML page. A Markdown, plain
// text or JSON response is kept as it is even when it quotes HTML.
func isHTMLResponse(contentType, text string) bool {
	if strings.Contains(contentType, "text/html") || strings.Contains(contentType, "xhtml") {
		return true
	}
	for _, kept := range []string{"text/markdown", "text/x-markdown", "text/plain", "json"} {
		if strings.Contains(contentType, kept) {
			return false
		}
	}
	return strings.Contains(text, "<html")
}

// isMarkdownResponse reports whether the site answered with Markdown, as sites do for an
// Accept: text/markdown request when they support it (Cloudflare's Markdown for Agents, docs
// platforms).
func isMarkdownResponse(contentType string) bool {
	return strings.Contains(contentType, "text/markdown") || strings.Contains(contentType, "text/x-markdown")
}
