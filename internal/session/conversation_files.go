package session

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"hmj-agent/v2/internal/config"
	"hmj-agent/v2/internal/logging"
	"hmj-agent/v2/internal/terminal/theme"
)

// The session directory holds one file per conversation, conversations/<id>.json, so two hmj
// processes in the same project each write only the conversation they have open, and state.json,
// with the conversation last active and the default model. An hmj locks the conversation it has
// open (conversation_lock.go); no other hmj opens, renames or deletes it meanwhile.
const (
	conversationsDirName = "conversations"
	stateFileName        = "state.json"
)

// savedState is the layout of state.json.
type savedState struct {
	ActiveID     string          `json:"activeId,omitempty"`
	DefaultModel config.ModelRef `json:"defaultModel"`
	// LastID is the highest conversation ID handed out, so the ID of a deleted conversation is
	// not given to a new one.
	LastID int `json:"lastId,omitzero"`
}

// fileStamp identifies a version of a conversation file, so a conversation another hmj has
// changed since it was read is read again.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func stampOf(fi os.FileInfo) fileStamp { return fileStamp{modTime: fi.ModTime(), size: fi.Size()} }

func (sm *SessionManager) conversationsDir() string {
	return filepath.Join(sm.dir, conversationsDirName)
}

func (sm *SessionManager) conversationPath(id string) string {
	return filepath.Join(sm.conversationsDir(), id+".json")
}

func (sm *SessionManager) lockPath(id string) string {
	return filepath.Join(sm.conversationsDir(), id+".lock")
}

// readState reads state.json. A missing or unreadable file gives the zero state: it only says
// where to start.
func (sm *SessionManager) readState() savedState {
	var st savedState
	data, err := os.ReadFile(filepath.Join(sm.dir, stateFileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logging.L().Warn("session state not read", "error", err.Error())
		}
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil {
		logging.L().Warn("session state not read", "error", err.Error())
		return savedState{}
	}
	return st
}

// writeState writes state.json. LastID only grows: another hmj may have handed out higher IDs.
func (sm *SessionManager) writeState() error {
	last := max(sm.counter, sm.readState().LastID)
	raw, err := json.Marshal(savedState{ActiveID: sm.ActiveID, DefaultModel: sm.DefaultModel, LastID: last})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(sm.dir, stateFileName), raw)
}

// readConversation reads the conversation file of id and finishes the conversation for use. A
// file that does not decode is moved aside as <id>.json.corrupt.<time>, with a warning.
func (sm *SessionManager) readConversation(id string) (*Conversation, fileStamp, error) {
	path := sm.conversationPath(id)
	f, err := os.Open(path)
	if err != nil {
		return nil, fileStamp{}, err
	}
	fi, statErr := f.Stat()
	var conv Conversation
	decodeErr := json.UnmarshalRead(f, &conv)
	if err := errors.Join(statErr, f.Close()); err != nil {
		return nil, fileStamp{}, err
	}
	if decodeErr != nil {
		setAside(path, id, decodeErr)
		return nil, fileStamp{}, decodeErr
	}
	conv.ID = id
	sm.finishLoaded(&conv)
	return &conv, stampOf(fi), nil
}

// setAside moves a conversation file that cannot be read to <path>.corrupt.<time> and says so.
func setAside(path, id string, reason error) {
	corruptPath := fmt.Sprintf("%s.corrupt.%s", path, time.Now().Format("20060102150405"))
	fmt.Fprintln(os.Stderr, theme.Caution(fmt.Sprintf("Conversation %s could not be read and was set aside.", id)))
	if err := os.Rename(path, corruptPath); err == nil {
		fmt.Fprintf(os.Stderr, "  The damaged file was kept as %s\n", corruptPath)
		pruneCorruptBackups(path)
	} else {
		fmt.Fprintf(os.Stderr, "  The damaged file could not be moved aside: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "  Reason: %v\n", reason)
}

// maxCorruptBackups is how many damaged copies of a conversation file are kept for recovery.
const maxCorruptBackups = 5

// pruneCorruptBackups deletes all but the newest maxCorruptBackups damaged copies of path.
// Their timestamp suffixes sort chronologically by name.
func pruneCorruptBackups(path string) {
	backups, err := filepath.Glob(path + ".corrupt.*")
	if err != nil || len(backups) <= maxCorruptBackups {
		return
	}
	slices.Sort(backups)
	for _, old := range backups[:len(backups)-maxCorruptBackups] {
		if err := os.Remove(old); err != nil {
			fmt.Fprintf(os.Stderr, "  An older damaged copy could not be removed: %v\n", err)
		}
	}
}

// writeConversation writes conv to its file and records the version written.
func (sm *SessionManager) writeConversation(conv *Conversation) error {
	// Compact encoding: conversations are machine-managed state (history is browsable in-app
	// via Ctrl+Y / /history), so smaller files and less marshal CPU win over readability.
	raw, err := json.Marshal(conv)
	if err != nil {
		return err
	}
	path := sm.conversationPath(conv.ID)
	if err := writeFileAtomic(path, raw); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil {
		sm.stamps[conv.ID] = stampOf(fi)
	}
	return nil
}

// writeFileAtomic replaces the file at path with data (mode 0600): it writes a temporary file
// in the same directory, syncs it and renames it over path, so a reader never sees half a
// file and two hmj processes never share a temporary name. Its errors name path, not the
// temporary file, so a failure that repeats reads the same each time.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	failed := func(err error, closeErr error) error {
		var pathErr *os.PathError
		var linkErr *os.LinkError
		switch {
		case errors.As(err, &linkErr):
			err = linkErr.Err
		case errors.As(err, &pathErr):
			err = pathErr.Err
		}
		return errors.Join(fmt.Errorf("writing %s: %w", path, err), closeErr, removeTemp(f.Name()))
	}
	if _, err := f.Write(data); err != nil {
		return failed(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return failed(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return failed(err, nil)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return failed(err, nil)
	}
	return nil
}

// staleTempAge is how old a temporary file of writeFileAtomic must be to count as left behind
// by a process that ended in the middle of a write; a write in progress takes far less.
const staleTempAge = time.Minute

// pruneTempFiles removes the temporary files writeFileAtomic leaves when the process ends between
// creating and renaming one, as a kill or a second signal during shutdown can make it, from the
// session directory and its conversations. One newer than staleTempAge may be another hmj's
// write in progress, so it is kept.
func (sm *SessionManager) pruneTempFiles() {
	now := time.Now()
	for _, dir := range []string{sm.dir, sm.conversationsDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.Type().IsRegular() || !strings.HasPrefix(name, ".") || !strings.Contains(name, ".json.tmp") {
				continue
			}
			if info, err := e.Info(); err != nil || now.Sub(info.ModTime()) < staleTempAge {
				continue
			}
			if err := removeTemp(filepath.Join(dir, name)); err != nil {
				logging.L().Warn("temporary file not removed", "error", err.Error())
			}
		}
	}
}

// removeTemp deletes a failed write's temporary file. A file that is already gone is not an
// error; anything else is reported alongside the write failure.
func removeTemp(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// refreshLocked brings the conversations in memory up to date with the session directory: it
// reads the conversations another hmj created or changed, and drops those it deleted. The
// conversation this hmj has open is its own, so its file is not read back.
func (sm *SessionManager) refreshLocked() {
	entries, err := os.ReadDir(sm.conversationsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logging.L().Warn("conversations not listed", "error", err.Error())
		return
	}
	onDisk := make(map[string]bool, len(entries))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		// Temporary files start with a dot; an ID that is not one path element names files
		// outside their roots (validID).
		if !ok || e.IsDir() || strings.HasPrefix(e.Name(), ".") || !validID(id) {
			continue
		}
		onDisk[id] = true
		if sm.isOpenHere(id) {
			continue
		}
		if fi, err := e.Info(); err == nil && sm.Conversations[id] != nil && sm.stamps[id] == stampOf(fi) {
			continue
		}
		conv, stamp, err := sm.readConversation(id)
		if err != nil {
			delete(sm.Conversations, id)
			delete(sm.stamps, id)
			continue
		}
		sm.Conversations[id] = conv
		sm.stamps[id] = stamp
	}
	for id := range sm.Conversations {
		if !onDisk[id] && !sm.isOpenHere(id) {
			delete(sm.Conversations, id)
			delete(sm.stamps, id)
		}
	}
}

// isOpenHere reports whether id is the conversation this hmj has open: the active one, which is
// this hmj's even while its lock could not be taken (Save takes it).
func (sm *SessionManager) isOpenHere(id string) bool {
	return id == sm.ActiveID
}

// nextID returns an ID above every conversation ID handed out (state.json's LastID), in use,
// saved or in memory, and every scratch directory and history file a conversation left behind,
// so a new conversation never takes a deleted one's number or inherits another's files.
func (sm *SessionManager) nextID() int {
	highest := sm.counter
	if sm.dir != "" {
		highest = max(highest, sm.readState().LastID)
	}
	note := func(name string) {
		if n, err := strconv.Atoi(name); err == nil && n > highest {
			highest = n
		}
	}
	for id := range sm.Conversations {
		note(id)
	}
	if sm.dir != "" {
		for _, sub := range []string{conversationsDirName, "scratch", "history"} {
			entries, err := os.ReadDir(filepath.Join(sm.dir, sub))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				// The O_EXCL create in claimConversation still keeps a new ID unique.
				logging.L().Warn("session directory not listed for IDs", "dir", sub, "error", err.Error())
			}
			for _, e := range entries {
				name, _, _ := strings.Cut(e.Name(), ".")
				note(name)
			}
		}
	}
	return highest + 1
}

// claimConversation creates the file of conv, a new conversation, under the first free ID from
// nextID, holding its lock, and sets conv.ID. The lock is taken before the file is created, so
// no other hmj can open the conversation before it is complete.
func (sm *SessionManager) claimConversation(conv *Conversation) (*conversationLock, error) {
	if err := os.MkdirAll(sm.conversationsDir(), 0o700); err != nil {
		return nil, err
	}
	for n := sm.nextID(); ; n++ {
		id := strconv.Itoa(n)
		lock, err := lockConversation(sm.lockPath(id), id)
		if errors.Is(err, ErrOpenElsewhere) {
			continue
		}
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(sm.conversationPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			if err := lock.release(); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, errors.Join(err, lock.release())
		}
		conv.ID = id
		sm.attachFiles(conv)
		conv.defaultName()
		raw, err := json.Marshal(conv)
		if err == nil {
			_, err = f.Write(raw)
		}
		if err == nil {
			err = f.Sync()
		}
		if err := errors.Join(err, f.Close()); err != nil {
			return nil, errors.Join(err, os.Remove(sm.conversationPath(id)), lock.release())
		}
		if fi, err := os.Stat(sm.conversationPath(id)); err == nil {
			sm.stamps[id] = stampOf(fi)
		}
		sm.counter = n
		return lock, nil
	}
}
