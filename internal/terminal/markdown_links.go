package terminal

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The model's [text](destination) links are shown by what they point to. A web page's link
// shows its text, which the terminal opens when clicked. A file's link shows its text and then
// the file's path, relative to the working directory when it is inside it: a terminal often
// cannot open a file:// URL (ChromeOS's opens it outside the Linux container), so the path is
// what lets the user find the file. The path is a link too, and like any linked word it is
// never cut by wrapping. Any other link is shown as written.

// markdownLink returns the [text](destination) link that s starts with and its length in
// bytes, or ok false. The destination has no spaces; brackets and parentheses may nest.
func markdownLink(s string) (text, dest string, n int, ok bool) {
	if !strings.HasPrefix(s, "[") {
		return "", "", 0, false
	}
	closeText := matching(s, 0, '[', ']')
	if closeText <= 1 || closeText+1 >= len(s) || s[closeText+1] != '(' {
		return "", "", 0, false
	}
	closeDest := matching(s, closeText+1, '(', ')')
	dest = s[closeText+2 : max(closeDest, closeText+2)]
	if closeDest < 0 || dest == "" || strings.ContainsAny(dest, " \t") {
		return "", "", 0, false
	}
	return s[1:closeText], dest, closeDest + 1, true
}

// matching returns the index of the close that pairs with the open at s[i], or -1.
func matching(s string, i int, open, close byte) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case open:
			depth++
		case close:
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// linkTarget returns what a link's destination points to: a web page's URL, or a file's URL and
// the path the transcript shows for it. Both are empty for any other destination, such as a
// mailto: address or an anchor in the same text.
func linkTarget(dest string) (webURL, fileURL, path string) {
	lower := strings.ToLower(dest)
	switch {
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return dest, "", ""
	case strings.HasPrefix(lower, "file://"):
		u, err := url.Parse(dest)
		if err != nil || u.Path == "" || u.Host != "" && u.Host != "localhost" {
			return "", "", ""
		}
		path = u.Path
		if u.Fragment != "" {
			path += "#" + u.Fragment
		}
	case dest[0] == '#' || strings.Contains(strings.SplitN(dest, "/", 2)[0], ":"):
		return "", "", "" // an anchor, or a scheme other than the web's and file's
	default:
		path = dest
		if p, err := url.PathUnescape(dest); err == nil {
			path = p
		}
	}
	file, fragment, _ := strings.Cut(path, "#")
	abs := file
	if !filepath.IsAbs(abs) {
		if wd, err := os.Getwd(); err == nil {
			abs = filepath.Join(wd, file)
		}
	}
	shown := file
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			shown = rel
		}
	}
	if fragment != "" {
		shown += "#" + fragment
	}
	return "", (&url.URL{Scheme: "file", Path: abs}).String(), shown
}
