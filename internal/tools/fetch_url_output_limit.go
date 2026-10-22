package tools

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// maxFetchFiles and maxFetchBytes bound the directory of saved fetch_url texts; the
	// oldest are removed first.
	maxFetchFiles = 100
	maxFetchBytes = 256 * 1024 * 1024
)

var (
	fetchMu  sync.Mutex
	fetchDir string
)

// SetFetchDir makes fetch_url save the full text of a page it truncates in dir, so the model
// can read the rest with read_file or search it with grep. An empty dir turns saving off, which
// is the default.
func SetFetchDir(dir string) {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	fetchDir = dir
}

// fetchResult is what fetch_url returns for text: the notes on how it was made, then the text
// as limitFetchedText cuts it.
func fetchResult(targetURL, text string, notes []string, maxLines int) string {
	out := limitFetchedText(targetURL, text, maxLines)
	if len(notes) > 0 {
		out = "[" + strings.Join(notes, "; ") + "]\n\n" + out
	}
	return out
}

// limitFetchedText returns the first maxLines lines of text, at most MaxFetchOutputBytes of
// them. When it cuts anything, it saves the whole text (see SetFetchDir) and ends the result
// with a note that names the saved file.
func limitFetchedText(targetURL, text string, maxLines int) string {
	lines := strings.Split(text, "\n")
	shown := lines[:min(len(lines), maxLines)]
	kept, lineCut := capLinesToBytes(shown, MaxFetchOutputBytes)
	if len(kept) == len(lines) && !lineCut {
		return text
	}

	var note string
	if lineCut {
		note = fmt.Sprintf("truncated to %s: line 1 of %d is %s long and was cut", formatSize(MaxFetchOutputBytes), len(lines), formatSize(len(lines[0])))
	} else {
		note = fmt.Sprintf("truncated to %d lines of %d, %s in all", len(kept), len(lines), formatSize(len(text)))
	}
	switch path, err := saveFetchedText(targetURL, text); {
	case err != nil:
		note += fmt.Sprintf("; the full text could not be saved: %v", err)
	case path != "":
		note += fmt.Sprintf("; the full text is saved to %s: read it with read_file (offset, limit) or search it with grep", path)
	case len(kept) == maxLines && maxLines < min(len(lines), maxFetchLines):
		note += "; use max_lines to view more"
	}
	return fmt.Sprintf("%s\n\n... [%s] ...", strings.Join(kept, "\n"), note)
}

// saveFetchedText writes text to the fetch directory under a name made of the time and the
// URL's host and path, and returns the file's path. It returns "" when saving is off.
func saveFetchedText(targetURL, text string) (string, error) {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	if fetchDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(fetchDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(fetchDir, time.Now().Format("20060102-150405.000000")+"_"+fetchFileName(targetURL)+".txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	pruneDir(fetchDir, maxFetchFiles, maxFetchBytes)
	return path, nil
}

// fetchFileName turns a URL's host and path into a file name of letters, digits, '.', '-' and
// '_', keeping the end of a long path, where the page's own name is.
func fetchFileName(targetURL string) string {
	name := targetURL
	if u, err := url.Parse(targetURL); err == nil {
		name = u.Host + u.Path
	}
	name = strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, name), "_")
	if len(name) > maxBackupNameBytes {
		name = name[len(name)-maxBackupNameBytes:]
	}
	return name
}

// formatSize writes n bytes as bytes, KB or MB.
func formatSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", n/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}
