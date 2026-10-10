package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildCLI builds the hmj binary into a temporary directory and returns its path. When
// HMJ_COVER_DIR is set (see `make cover`), the binary is built with coverage
// instrumentation so the subprocess tests count towards the merged coverage report;
// cliEnv then points the child's GOCOVERDIR at that directory.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hmj")
	args := []string{"build", "-trimpath", "-o", bin}
	if os.Getenv("HMJ_COVER_DIR") != "" {
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=hmj-agent/...")
	}
	args = append(args, "hmj-agent")
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v, out: %s", err, out)
	}
	return bin
}

// cliEnv is the environment for a binary from buildCLI: the test's own environment plus,
// under HMJ_COVER_DIR, the GOCOVERDIR the instrumented binary writes its counters to.
func cliEnv() []string {
	env := os.Environ()
	if dir := os.Getenv("HMJ_COVER_DIR"); dir != "" {
		env = append(env, "GOCOVERDIR="+dir)
	}
	return env
}
