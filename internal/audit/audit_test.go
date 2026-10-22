package audit

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.log")
	Record("ignored", "{}", time.Second, 1, nil) // before Init: dropped
	closeFn, err := Init(path, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	Record("run_command", `{"command":"export KEY=sk-proj-0123456789abcdefghijk; make"}`, 1500*time.Millisecond, 42, nil)
	Record("read_file", `{"path":"/missing"}`, time.Millisecond, 0, errors.New("no such file"))

	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("audit log mode: %v, %v", info, err)
	}
	entries := readEntries(t, path)
	if len(entries) != 3 || entries[0].Event != "start" || entries[0].Version != "1.2.3" || entries[0].Dir == "" {
		t.Fatalf("entries = %+v", entries)
	}
	run := entries[1]
	if run.Tool != "run_command" || run.DurationMS != 1500 || run.OutputBytes != 42 || run.Error != "" {
		t.Errorf("run entry = %+v", run)
	}
	if !strings.Contains(run.Args, "KEY=sk-proj-0123456789abcdefghijk; make") {
		t.Errorf("args not recorded as given: %q", run.Args)
	}
	if entries[2].Error != "no such file" {
		t.Errorf("error not recorded: %+v", entries[2])
	}
}

func TestRotation(t *testing.T) {
	old := maxFileBytes
	maxFileBytes = 2000
	t.Cleanup(func() { maxFileBytes = old })
	path := filepath.Join(t.TempDir(), "audit.log")
	closeFn, err := Init(path, "v")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	for range 50 {
		Record("list_files", `{"path":"`+strings.Repeat("d", 100)+`"}`, 0, 0, nil)
	}
	for _, p := range []string{path, path + ".1"} {
		info, err := os.Stat(p)
		if err != nil || info.Size() > maxFileBytes {
			t.Errorf("%s: %v, %v", p, info, err)
		}
	}
	if len(readEntries(t, path)) == 0 {
		t.Error("current log is empty after rotation")
	}
}
