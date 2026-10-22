package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"hmj-agent/internal/chat"
)

type FetchURLTool struct {
	Client *http.Client
}

func (t *FetchURLTool) Name() string { return "fetch_url" }

func (t *FetchURLTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name: "fetch_url",
			Description: "Fetch and read text content from a web page or documentation URL, converting HTML to clean plain text. " +
				"It asks the site for Markdown first, shows GitHub file pages as the raw file, decodes base64 file sources " +
				"(googlesource ?format=TEXT) and indents JSON. On documentation sites, <site>/llms.txt often lists the pages " +
				"to read, <site>/llms-full.txt holds all of them, and many pages are served as Markdown at their URL plus .md. " +
				"Pages from public hosts are kept for 15 minutes, so fetching one again returns that copy; local and " +
				"private hosts are fetched every time. " +
				"For downloading binary files (executables, archives, images), use run_command with curl instead.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The HTTP or HTTPS URL to fetch.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional seconds to wait for the whole response (default 20, max 120).",
					},
					"max_lines": map[string]any{
						"type":        "integer",
						"description": "Optional maximum number of lines to return (default 120, max 1000). Output is also capped at 64 KB; a page cut short is saved to a file whose path the result names.",
					},
				},
				"required":             []string{"url"},
				"additionalProperties": false,
			},
		},
	}
}

type fetchURLInput struct {
	URL            string `json:"url"`
	MaxLines       int    `json:"max_lines"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

const (
	// maxRedirects bounds how many HTTP redirects fetch_url will follow.
	maxRedirects = 10
	// defaultFetchLines and maxFetchLines are the default and largest max_lines.
	defaultFetchLines = 120
	maxFetchLines     = 1000
	// defaultFetchTimeout and maxFetchTimeout are the default and largest timeout_seconds.
	defaultFetchTimeout = 20 * time.Second
	maxFetchTimeout     = 120 * time.Second
)

// fetchClient returns the package-level pooled HTTP client used when
// FetchURLTool.Client is nil. It is created once and shared across calls.
var fetchClient = sync.OnceValue(newFetchClient)

func newFetchClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			MaxIdleConns:          20,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
		// Each request also has its own deadline, timeout_seconds; this only bounds a caller
		// that sets none.
		Timeout: maxFetchTimeout,
		// Follow redirects but never into non-HTTP(S) schemes (file:, gopher:, etc.)
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to non-HTTP scheme %q blocked", req.URL.Scheme)
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

func (t *FetchURLTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var in fetchURLInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	targetURL := strings.TrimSpace(in.URL)
	if targetURL == "" {
		return "", errors.New("url is required")
	}

	parsedURL, err := url.Parse(targetURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return "", fmt.Errorf("invalid URL %q: must start with http:// or https://", targetURL)
	}

	// notes tell the model how the response was turned into the text it gets.
	var notes []string
	if raw := rawGitHubURL(parsedURL); raw != "" {
		notes = append(notes, "fetched the raw file "+raw+" instead of its GitHub page")
		targetURL = raw
		parsedURL, _ = url.Parse(raw)
	}

	maxLines := in.MaxLines
	if maxLines <= 0 {
		maxLines = defaultFetchLines
	} else if maxLines > maxFetchLines {
		maxLines = maxFetchLines
	}

	if page, ok := cachedFetch(parsedURL.String()); ok {
		return fetchResult(targetURL, page.text, append(append(notes, page.notes...), page.ageNote()), maxLines), nil
	}
	// The notes from here on describe the response, and are cached with its text.
	responseNotes := len(notes)

	timeout := defaultFetchTimeout
	if in.TimeoutSeconds > 0 {
		timeout = min(time.Duration(in.TimeoutSeconds)*time.Second, maxFetchTimeout)
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// timedOut explains an error caused by the deadline, as opposed to a halt or a network error.
	timedOut := func(err error) error {
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("no complete response within %s; set timeout_seconds (up to %d) to wait longer: %w",
				timeout, int(maxFetchTimeout/time.Second), err)
		}
		return err
	}

	client := t.Client
	if client == nil {
		client = fetchClient()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", browserUserAgent)
	// Markdown first: sites that can serve it (Cloudflare's Markdown for Agents, docs
	// platforms) answer with far fewer tokens than their HTML.
	req.Header.Set("Accept", "text/markdown,text/html;q=0.9,application/xhtml+xml;q=0.9,application/xml;q=0.8,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", timedOut(err))
	}
	defer resp.Body.Close()
	if final := resp.Request.URL; !strings.EqualFold(final.Host, parsedURL.Host) {
		notes = append(notes, "redirected to "+final.String())
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded amount of the error body so the pooled connection
		// can be reused instead of being torn down unread.
		_, drainErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		if drainErr != nil {
			return fmt.Sprintf("[HTTP %s for %s; response drain failed: %v]", resp.Status, targetURL, drainErr), nil
		}
		return fmt.Sprintf("[HTTP %s for %s]", resp.Status, targetURL), nil
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchBodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", timedOut(err))
	}

	// Detect binary content
	if isBinaryContent(contentType, body) {
		return fmt.Sprintf("[binary content (%s, %d bytes) detected at %s. To download binaries, use run_command: 'curl -fsSL -o <filename> %s']",
			contentType, len(body), targetURL, targetURL), nil
	}

	var text string
	if decoded, ok := decodeBase64Text(parsedURL, contentType, body); ok {
		text = decoded
		notes = append(notes, "decoded from base64")
	} else if indented, ok := indentJSON(contentType, body); ok {
		text = indented
	} else if isHTMLResponse(contentType, string(body)) {
		text = htmlToText(string(body))
	} else {
		text = string(body)
		if isMarkdownResponse(contentType) {
			notes = append(notes, "the site served Markdown")
		}
	}

	text = strings.TrimSpace(text)
	storeFetch(parsedURL, text, notes[responseNotes:])
	return fetchResult(targetURL, text, notes, maxLines), nil
}

func isBinaryContent(contentType string, data []byte) bool {
	binaryTypes := []string{
		"application/octet-stream", "application/zip", "application/gzip",
		"application/x-tar", "application/x-executable", "application/pdf",
		"image/", "audio/", "video/", "font/",
	}
	for _, bt := range binaryTypes {
		if strings.Contains(contentType, bt) {
			return true
		}
	}
	// Check first 512 bytes for null byte
	checkLen := min(len(data), 512)
	return bytes.IndexByte(data[:checkLen], 0) != -1
}
