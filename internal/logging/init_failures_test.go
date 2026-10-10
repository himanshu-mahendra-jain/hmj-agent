package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Init reports paths it cannot create and leaves the discard logger in place.
func TestInitFailuresKeepDiscardLogger(t *testing.T) {
	resetLogger(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"parent is a regular file", filepath.Join(blocker, "debug.log")},
		{"path is a directory", dir},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if closeFn, err := Init(tc.path); err == nil {
				closeFn()
				t.Fatalf("Init(%q) succeeded, want an error", tc.path)
			}
			if L().Enabled(context.Background(), slog.LevelError) {
				t.Error("a failed Init left a real logger in place")
			}
		})
	}
}
