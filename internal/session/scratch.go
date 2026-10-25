package session

import (
	"fmt"
	"hmj-agent/v2/internal/logging"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// A scratch directory holds one conversation's temporary files: logs, saved command output,
// copies of files and throwaway scripts, which would otherwise go to /tmp under names that
// other sessions reuse, or into the project. Tests lower these limits.
var (
	// scratchMaxIdle is how long a scratch directory is kept after its last change.
	scratchMaxIdle = 7 * 24 * time.Hour
	// orphanScratchMaxIdle is how long the directory of a conversation without a conversation
	// file is kept: another hmj may be creating the conversation.
	orphanScratchMaxIdle = time.Hour
)

// ScratchPrompt is the system prompt's paragraph naming the conversation's scratch directory.
func ScratchPrompt(dir string) string {
	return fmt.Sprintf("\n\nYour scratch directory for this conversation is %s (also $HMJ_SCRATCH in run_command). "+
		"Put temporary files there, such as logs, saved command output, copies of files and throwaway scripts, "+
		"not in /tmp or the project. It is kept while the conversation exists, until 7 days after its last change. "+
		"When you tell the user about a file there, give its full path: $HMJ_SCRATCH is not set in the user's shell.", dir)
}

// scratchRoot returns the directory that holds the conversations' scratch directories, next to
// the conversation files, or "" when the conversations are not saved.
func (sm *SessionManager) scratchRoot() string {
	if sm.dir == "" {
		return ""
	}
	return filepath.Join(sm.dir, "scratch")
}

// attachFiles sets conv's scratch directory and history file from the session directory.
func (sm *SessionManager) attachFiles(conv *Conversation) {
	conv.ScratchDir = ""
	if root := sm.scratchRoot(); root != "" {
		conv.ScratchDir = filepath.Join(root, conv.ID)
	}
	conv.HistoryPath = sm.historyPath(conv.ID)
}

// pruneScratch removes the scratch directories not changed for scratchMaxIdle, unless another hmj
// has the conversation open, and those of conversations without a conversation file once
// orphanScratchMaxIdle has passed.
func (sm *SessionManager) pruneScratch() {
	root := sm.scratchRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		changed, err := lastChange(dir)
		if err != nil {
			logging.L().Warn("scratch directory unreadable, kept", "dir", dir, "error", err.Error())
			continue
		}
		idle := now.Sub(changed)
		_, exists := sm.Conversations[e.Name()]
		if exists && idle > scratchMaxIdle && sm.openElsewhereLocked(e.Name()) {
			continue
		}
		if idle > scratchMaxIdle || !exists && idle > orphanScratchMaxIdle {
			if err := os.RemoveAll(dir); err != nil {
				logging.L().Warn("idle scratch directory not removed", "dir", dir, "error", err.Error())
			}
		}
	}
}

// lastChange returns the newest modification time of dir and everything in it. Entries that
// cannot be read are passed over; dir itself not being readable is an error, since its age
// is then unknown.
func lastChange(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}
