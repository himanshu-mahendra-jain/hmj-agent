// Package audit keeps an append-only record of every tool the agent runs, as JSON
// lines. Unlike the opt-in diagnostic log it is meant to be always on: the agent
// opens it at startup next to its config. Arguments are capped, and
// the file is rotated once it grows past maxFileBytes.
package audit

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hmj-agent/internal/logging"
)

// maxArgBytes caps the recorded tool arguments.
const maxArgBytes = 8 * 1024

// maxFileBytes is the size at which the log is moved to <path>.1, replacing any
// previous one, and a new log is started. A variable so tests can lower it.
var maxFileBytes int64 = 10 * 1024 * 1024

var (
	mu   sync.Mutex
	path string
	file *os.File
)

// Entry is one audit record.
type Entry struct {
	Time        time.Time `json:"time"`
	Event       string    `json:"event"` // "start" or "tool"
	Tool        string    `json:"tool,omitempty"`
	Args        string    `json:"args,omitempty"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	OutputBytes int       `json:"output_bytes,omitempty"`
	Error       string    `json:"error,omitempty"`
	Dir         string    `json:"dir,omitempty"`
	Version     string    `json:"version,omitempty"`
}

// Init opens the audit log at p (creating its directory with mode 0700 and the file
// with 0600) and records a start entry. Until Init succeeds, Record does nothing.
// The returned func closes the log.
func Init(p, version string) (closeFn func() error, err error) {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		if err := file.Close(); err != nil {
			logging.L().Warn("audit log close failed", "path", path, "error", err.Error())
		}
		file = nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	path, file = p, f
	dir, err := os.Getwd()
	if err != nil {
		dir = "unknown: " + err.Error()
	}
	writeLocked(Entry{Event: "start", Dir: dir, Version: version})
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if file != f {
			return nil
		}
		file = nil
		return f.Close()
	}, nil
}

// Record logs one tool execution. execErr is nil when the tool succeeded.
func Record(tool, args string, d time.Duration, outputBytes int, execErr error) {
	e := Entry{
		Event:       "tool",
		Tool:        tool,
		Args:        logging.Truncate(args, maxArgBytes),
		DurationMS:  d.Milliseconds(),
		OutputBytes: outputBytes,
	}
	if execErr != nil {
		e.Error = execErr.Error()
	}
	mu.Lock()
	defer mu.Unlock()
	writeLocked(e)
}

// writeLocked appends e, rotating the file first if it is full. Write failures are
// reported to the diagnostic log only: auditing must never break a turn.
func writeLocked(e Entry) {
	if file == nil {
		return
	}
	e.Time = time.Now().UTC()
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	if info, err := file.Stat(); err == nil && info.Size()+int64(len(line)) > maxFileBytes {
		if rotateLocked(); file == nil {
			return
		}
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		logging.L().Warn("audit log write failed", "path", path, "error", err.Error())
	}
}

func rotateLocked() {
	if err := file.Close(); err != nil {
		logging.L().Warn("audit log close failed", "path", path, "error", err.Error())
	}
	if err := os.Rename(path, path+".1"); err != nil {
		logging.L().Warn("audit log rotation failed", "path", path, "error", err.Error())
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logging.L().Warn("audit log reopen failed", "path", path, "error", err.Error())
		file = nil
		return
	}
	file = f
}
