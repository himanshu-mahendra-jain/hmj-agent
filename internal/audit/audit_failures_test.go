package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Init refuses paths it cannot create, and a failed Init leaves auditing off rather
// than writing to the previous file.
func TestInitFailures(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"parent is a regular file", filepath.Join(blocker, "audit.log")},
		{"path is a directory", dir},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if closeFn, err := Init(tc.path, "v"); err == nil {
				closeFn()
				t.Fatalf("Init(%q) succeeded, want an error", tc.path)
			}
			Record("read_file", "{}", 0, 0, nil) // must not panic or write anywhere
		})
	}
}

// Calling Init again switches to the new file; the first close func then does nothing,
// because the file it would close was already replaced.
func TestInitTwiceSwitchesFiles(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")
	closeA, err := Init(first, "v")
	if err != nil {
		t.Fatal(err)
	}
	closeB, err := Init(second, "v")
	if err != nil {
		t.Fatal(err)
	}
	if err := closeA(); err != nil {
		t.Errorf("stale close func returned %v", err)
	}
	Record("list_files", "{}", time.Millisecond, 3, nil)
	if err := closeB(); err != nil {
		t.Fatal(err)
	}
	if n := len(readEntries(t, first)); n != 1 {
		t.Errorf("first log has %d entries, want only its start entry", n)
	}
	if got := readEntries(t, second); len(got) != 2 || got[1].Tool != "list_files" {
		t.Errorf("second log = %+v, want start + list_files", got)
	}
}

// A write that fails (here a full disk) is reported to the diagnostic log, never to the
// caller: auditing must not break a turn.
func TestRecordWriteFailureIsSwallowed(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full not available")
	}
	closeFn, err := Init("/dev/full", "v")
	if err != nil {
		t.Skipf("cannot open /dev/full: %v", err)
	}
	defer closeFn()
	Record("run_command", `{"command":"true"}`, 0, 0, nil)
}

// When rotation cannot move the log aside or reopen it (the directory and file became
// read-only), auditing switches itself off instead of failing every later record.
func TestRotationFailureDisablesAuditing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	old := maxFileBytes
	maxFileBytes = 300
	t.Cleanup(func() { maxFileBytes = old })
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "audit.log")
	closeFn, err := Init(path, "v")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	for range 5 {
		Record("list_files", `{"path":"`+strings.Repeat("d", 100)+`"}`, 0, 0, nil)
	}
	mu.Lock()
	off := file == nil
	mu.Unlock()
	if !off {
		t.Error("auditing still on after the log could not be reopened")
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("rotated file exists although the directory is read-only: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Errorf("close after disabling: %v", err)
	}
}

// If the log is deleted while open, rotation cannot rename it but starts a fresh file at
// the same path, so later records are not lost.
func TestRotationAfterLogDeleted(t *testing.T) {
	old := maxFileBytes
	maxFileBytes = 300
	t.Cleanup(func() { maxFileBytes = old })
	path := filepath.Join(t.TempDir(), "audit.log")
	closeFn, err := Init(path, "v")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		Record("read_file", `{"path":"`+strings.Repeat("x", 100)+`"}`, 0, 0, nil)
	}
	if got := readEntries(t, path); len(got) == 0 || got[len(got)-1].Tool != "read_file" {
		t.Errorf("recreated log = %+v, want the later records", got)
	}
}
