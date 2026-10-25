package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"hmj-agent/v2/internal/logging"
)

// ErrOpenElsewhere matches, via errors.Is, opening, renaming or deleting a conversation that
// another hmj has open.
var ErrOpenElsewhere = errors.New("open in another hmj")

// openElsewhereError names the conversation and, when its lock file says, the other hmj's
// process ID.
type openElsewhereError struct {
	id  string
	pid int
}

func (e openElsewhereError) Error() string {
	if e.pid > 0 {
		return fmt.Sprintf("conversation %s is open in another hmj (pid %d)", e.id, e.pid)
	}
	return fmt.Sprintf("conversation %s is open in another hmj", e.id)
}

func (e openElsewhereError) Is(target error) bool { return target == ErrOpenElsewhere }

// conversationLock holds the write lock on a conversation's lock file, <id>.lock beside its
// conversation file, while an hmj has the conversation open. It is an open file description
// lock (F_OFD_SETLK): it belongs to the open file, not the process, so two SessionManagers in one
// process exclude each other as two hmj processes do, and the kernel releases it when hmj exits,
// however it ends. The file holds the holder's process ID for the message another hmj shows.
type conversationLock struct {
	f *os.File
}

// lockConversation takes the lock at path for the conversation id without waiting. It fails
// with an error matching ErrOpenElsewhere when another open file holds it.
func lockConversation(path, id string) (*conversationLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart}
	if err := unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &lk); err != nil {
		pid := lockHolder(f)
		closeErr := f.Close()
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			return nil, openElsewhereError{id: id, pid: pid}
		}
		return nil, errors.Join(fmt.Errorf("locking conversation %s: %w", id, err), closeErr)
	}
	// The ID only improves the other hmj's message; a lock without it still excludes.
	if err := f.Truncate(0); err != nil {
		logging.L().Warn("conversation lock holder not recorded", "conversation", id, "error", err.Error())
	} else if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		logging.L().Warn("conversation lock holder not recorded", "conversation", id, "error", err.Error())
	}
	return &conversationLock{f: f}, nil
}

// release gives up the lock. A nil lock is released already.
func (l *conversationLock) release() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}

// lockedElsewhere reports whether another open file holds the lock at path, without taking
// it, and the holder's process ID when the file records one.
func lockedElsewhere(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart}
	if unix.FcntlFlock(f.Fd(), unix.F_OFD_GETLK, &lk) != nil || lk.Type == unix.F_UNLCK {
		return 0, false
	}
	return lockHolder(f), true
}

// lockHolder returns the process ID recorded in a lock file, or 0.
func lockHolder(f *os.File) int {
	buf := make([]byte, 20)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) { // EOF: the whole short file was read
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
