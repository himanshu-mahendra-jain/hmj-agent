package tools

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// commandHeadBytes and commandTailBytes are how much of a long command output run_command
	// returns: its start, where the command says what it is doing, and its end, where builds
	// and test runners report failures. Together they make MaxCommandOutputBytes.
	commandHeadBytes = 16 * 1024
	commandTailBytes = MaxCommandOutputBytes - commandHeadBytes
	// maxCommandLogFiles and maxCommandLogDirBytes bound the saved outputs' directory, the
	// oldest files going first.
	maxCommandLogFiles    = 100
	maxCommandLogDirBytes = 256 * 1024 * 1024
)

var (
	commandLogMu  sync.Mutex
	commandLogDir string
	// maxCommandLogBytes caps one saved output. Tests lower it.
	maxCommandLogBytes = 64 * 1024 * 1024
)

// SetCommandLogDir makes run_command save the whole output of a command that outgrows what it
// returns in dir, so the model can read the middle with read_file or search it with grep. An
// empty dir turns saving off, which is the default.
func SetCommandLogDir(dir string) {
	commandLogMu.Lock()
	defer commandLogMu.Unlock()
	commandLogDir = dir
}

// commandOutput collects a command's stdout and stderr as one stream, in the order written:
// run_command gives it to the command as both, so they share one pipe, as with 2>&1. Up to
// MaxCommandOutputBytes it keeps everything. Past that it keeps the first commandHeadBytes and
// the latest commandTailBytes, and writes the whole output to a log file (see SetCommandLogDir).
type commandOutput struct {
	mu      sync.Mutex
	command string
	head    []byte // everything until the output outgrows MaxCommandOutputBytes, then its start
	tail    []byte // past that, the latest output: at least commandTailBytes+1 once it has them
	total   int
	long    bool // the output outgrew MaxCommandOutputBytes

	log      *os.File
	logPath  string
	logErr   error
	logBytes int

	// report, when set, hears the latest output line after each write.
	report func(string)
	lines  lastLineTracker
}

func newCommandOutput(command string, report func(string)) *commandOutput {
	return &commandOutput{command: command, report: report}
}

func (o *commandOutput) Write(p []byte) (int, error) {
	line := o.write(p)
	if o.report != nil {
		o.report(line) // outside the lock: the terminal may take a moment
	}
	return len(p), nil
}

// write keeps p and returns the latest output line for a progress report.
func (o *commandOutput) write(p []byte) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var line string
	if o.report != nil {
		line = o.lines.Write(p)
	}
	o.total += len(p)
	if !o.long {
		o.head = append(o.head, p...)
		if len(o.head) <= MaxCommandOutputBytes {
			return line
		}
		// From here on only the start and the end are kept; the log gets everything.
		o.long = true
		all := o.head
		o.head = all[:commandHeadBytes:commandHeadBytes]
		o.tail = append([]byte(nil), all[commandHeadBytes:]...)
		o.openLog()
		o.writeLog(all)
	} else {
		o.tail = append(o.tail, p...)
		o.writeLog(p)
	}
	// Trim the tail now and then rather than on every write; one byte more than shown is kept
	// to tell whether the shown part starts a line.
	if len(o.tail) > 2*commandTailBytes {
		o.tail = append([]byte(nil), o.tail[len(o.tail)-commandTailBytes-1:]...)
	}
	return line
}

// openLog creates the log file for a long output. It leaves logPath empty when saving is off.
func (o *commandOutput) openLog() {
	commandLogMu.Lock()
	dir := commandLogDir
	commandLogMu.Unlock()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		o.logErr = err
		return
	}
	path := filepath.Join(dir, time.Now().Format("20060102-150405.000000")+"_"+commandLogName(o.command)+".log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		o.logErr = err
		return
	}
	o.log, o.logPath = f, path
}

// writeLog appends p to the log, up to maxCommandLogBytes in all.
func (o *commandOutput) writeLog(p []byte) {
	if o.log == nil || o.logErr != nil {
		return
	}
	p = p[:min(len(p), maxCommandLogBytes-o.logBytes)]
	n, err := o.log.Write(p)
	o.logBytes += n
	if err != nil {
		o.logErr = err
	}
}

// finish closes the log and returns the output as run_command reports it: all of it when it
// fits, else its start and end around a note of what was left out and where it was saved.
// Trailing line breaks are dropped.
func (o *commandOutput) finish() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.log != nil {
		if err := o.log.Close(); err != nil && o.logErr == nil {
			o.logErr = err
		}
		o.log = nil
		commandLogMu.Lock()
		dir := commandLogDir
		commandLogMu.Unlock()
		pruneDir(dir, maxCommandLogFiles, maxCommandLogDirBytes)
	}
	if !o.long {
		return strings.TrimRight(strings.ToValidUTF8(string(o.head), ""), "\r\n")
	}

	head := o.head
	if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
		head = head[:i] // end on a whole line
	}
	tail := o.tail
	if len(tail) > commandTailBytes {
		startsLine := tail[len(tail)-commandTailBytes-1] == '\n'
		tail = tail[len(tail)-commandTailBytes:]
		if i := bytes.IndexByte(tail, '\n'); !startsLine && i >= 0 {
			tail = tail[i+1:] // start on a whole line
		}
	}
	note := fmt.Sprintf("%s of %s omitted", formatSize(o.total-len(head)-len(tail)), formatSize(o.total))
	switch {
	case o.logErr != nil && o.logPath == "":
		note += fmt.Sprintf("; the full output could not be saved: %v", o.logErr)
	case o.logPath != "":
		note += fmt.Sprintf("; the full output is saved to %s: read it with read_file (offset, limit) or search it with grep", o.logPath)
		if o.logErr != nil {
			note += fmt.Sprintf(" (saving stopped early: %v)", o.logErr)
		} else if o.logBytes < o.total {
			note += fmt.Sprintf(" (its first %s)", formatSize(o.logBytes))
		}
	}
	return fmt.Sprintf("%s\n\n... [%s] ...\n\n%s",
		strings.ToValidUTF8(string(head), ""), note, strings.TrimRight(strings.ToValidUTF8(string(tail), ""), "\r\n"))
}

// commandLogName turns the start of a command into a file name of letters, digits, '.', '-'
// and '_'.
func commandLogName(command string) string {
	name := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, command), "_")
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "_")
	}
	return cmp.Or(name, "command")
}
