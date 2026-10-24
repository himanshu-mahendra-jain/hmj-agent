package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"../../internal/lint"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Errorf("clean package: exit %d, output %q %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	if code := run([]string{"./does/not/exist"}, &stdout, &stderr); code != 2 {
		t.Errorf("missing package: exit %d, want 2", code)
	}

	dir := filepath.Join(t.TempDir(), "drop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	writeFile(t, "go.mod", "module drop\n\ngo 1.27\n")
	writeFile(t, "drop.go", "package drop\n\nimport \"os\"\n\nfunc F() { os.Remove(\"x\") }\n")
	stdout.Reset()
	if code := run(nil, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "drop.go:5:12: error from os.Remove not checked") {
		t.Errorf("dropped error: exit %d, output %q", code, stdout.String())
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
