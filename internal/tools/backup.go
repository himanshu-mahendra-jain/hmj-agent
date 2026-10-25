package tools

import (
	"fmt"
	"hmj-agent/v2/internal/logging"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// maxBackups and maxBackupBytes bound the backup directory; the oldest
	// backups are removed first.
	maxBackups     = 200
	maxBackupBytes = 256 * 1024 * 1024
	// maxBackupNameBytes keeps backup file names under the 255-byte limit.
	maxBackupNameBytes = 200
)

var (
	backupMu  sync.Mutex
	backupDir string
)

// SetBackupDir makes edit_file and write_file save a copy of a file's previous content
// in dir before changing it. An empty dir turns backups off, which is the default.
func SetBackupDir(dir string) {
	backupMu.Lock()
	defer backupMu.Unlock()
	backupDir = dir
}

// backupFile saves old, the current content of absPath, to the backup directory. The
// name is a timestamp followed by the path with "/" written as "%", so backups sort
// oldest first and name the file they came from. It returns a note for the tool result
// when the backup could not be saved; the edit goes ahead regardless.
func backupFile(absPath string, old []byte) string {
	backupMu.Lock()
	defer backupMu.Unlock()
	if backupDir == "" {
		return ""
	}
	if err := writeBackup(absPath, old); err != nil {
		return fmt.Sprintf("\n\n[warning: no backup of the previous content was saved: %v]", err)
	}
	return ""
}

func writeBackup(absPath string, old []byte) error {
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	encoded := strings.ReplaceAll(strings.TrimPrefix(absPath, "/"), "/", "%")
	if len(encoded) > maxBackupNameBytes {
		encoded = encoded[len(encoded)-maxBackupNameBytes:] // keep the file name end
	}
	name := time.Now().Format("20060102-150405.000000") + "_" + encoded
	if err := os.WriteFile(filepath.Join(backupDir, name), old, 0o600); err != nil {
		return err
	}
	pruneDir(backupDir, maxBackups, maxBackupBytes)
	return nil
}

// pruneDir removes the oldest files of dir beyond maxFiles files or maxBytes in total. File
// names must start with a timestamp, so they sort by age. Failures are ignored: an
// oversized directory only costs disk space.
func pruneDir(dir string, maxFiles int, maxBytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(b.Name(), a.Name()) })
	var total int64
	for i, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		if i >= maxFiles || total > maxBytes {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				logging.L().Warn("old backup not removed", "file", e.Name(), "error", err.Error())
			}
		}
	}
}
