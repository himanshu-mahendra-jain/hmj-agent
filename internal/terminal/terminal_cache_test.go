package terminal

import (
	"os"
	"testing"
)

// TestCachedIsTerminalMemoizesPerDescriptor checks that a pipe is reported as not a terminal and that
// the answer is stable across calls, since it is served from the cache after the first check.
func TestCachedIsTerminalMemoizesPerDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	for range 3 {
		if cachedIsTerminal(w.Fd()) {
			t.Fatal("a pipe must not be reported as a terminal")
		}
	}
}
