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
				"For downloading binary files (executables, archives, images), use run_command with curl instead.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The HTTP or HTTPS URL to fetch.",
					},
					"max_lines": map[string]any{
						"type":        "integer",
						"description": "Optional maximum number of lines to return (default 120, max 1000).",
					},
				},
				"required":             []string{"url"},
				"additionalProperties": false,
			},
		},
	}
}

type fetchURLInput struct {
	URL      string `json:"url"`
	MaxLines int    `json:"max_lines"`
}

// maxRedirects bounds how many HTTP redirects fetch_url will follow.
const maxRedirects = 10

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
		Timeout: 20 * time.Second,
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

	maxLines := in.MaxLines
	if maxLines <= 0 {
		maxLines = 120
	} else if maxLines > 1000 {
		maxLines = 1000
	}

	client := t.Client
	if client == nil {
		client = fetchClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

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
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Detect binary content
	if isBinaryContent(contentType, body) {
		return fmt.Sprintf("[binary content (%s, %d bytes) detected at %s. To download binaries, use run_command: 'curl -fsSL -o <filename> %s']",
			contentType, len(body), targetURL, targetURL), nil
	}

	text := string(body)
	if strings.Contains(contentType, "text/html") || strings.Contains(contentType, "xhtml") || strings.Contains(text, "<html") {
		text = htmlToText(text)
	}

	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		truncated := strings.Join(lines[:maxLines], "\n")
		return fmt.Sprintf("%s\n\n... [truncated to %d lines of %d total; use max_lines to view more] ...",
			truncated, maxLines, len(lines)), nil
	}

	return strings.TrimSpace(text), nil
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
