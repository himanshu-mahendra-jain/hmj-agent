// Package logging owns the optional diagnostic log. Nothing is written unless
// Init is called with a path; the agent wires that to the HMJ_LOG variable.
package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

var (
	discard = slog.New(slog.DiscardHandler)

	// logger holds the current logger; it is nil until the first Init call.
	logger atomic.Pointer[slog.Logger]

	// mu serialises Init and the close functions, which swap logger and file together.
	mu   sync.Mutex
	file *os.File
)

// L returns the current logger. It discards records until Init opens a file.
func L() *slog.Logger {
	if l := logger.Load(); l != nil {
		return l
	}
	return discard
}

// Init routes the logger to a JSON-lines file at path, created in append mode
// (its parent directory is created too). An empty path leaves the discard logger
// in place. Calling Init again closes the previous file first. The returned func
// stops logging to the file and closes it.
func Init(path string) (closeFn func() error, err error) {
	mu.Lock()
	defer mu.Unlock()

	if err := closeCurrentLocked(); err != nil {
		return nil, err
	}
	if path == "" {
		return func() error { return nil }, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	file = f
	logger.Store(slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() error { return closeFile(f) }, nil
}

// closeCurrentLocked switches back to the discard logger and closes the open file,
// returning the close error. The caller must hold mu.
func closeCurrentLocked() error {
	logger.Store(discard)
	if file == nil {
		return nil
	}
	err := file.Close()
	file = nil
	return err
}

// closeFile closes f unless Init has already replaced it, in which case it was closed then.
func closeFile(f *os.File) error {
	mu.Lock()
	defer mu.Unlock()
	if file != f {
		return nil
	}
	return closeCurrentLocked()
}

// Truncate shortens s to at most maxBytes bytes, ending with "..." when it cuts.
// The cut falls on a rune boundary, so the result is always valid UTF-8. A budget
// too small to hold the ellipsis yields the empty string.
func Truncate(s string, maxBytes int) string {
	const ellipsis = "..."
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes < len(ellipsis) {
		return ""
	}
	cut := maxBytes - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
