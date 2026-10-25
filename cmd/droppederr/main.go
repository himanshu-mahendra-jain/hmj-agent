// Command droppederr reports errors dropped in the packages its arguments name (default
// ./...): assigned to the blank identifier or returned by a call made as a statement (see
// lint.DroppedErrors). It exits 1 if there are any. `make check` runs it.
package main

import (
	"fmt"
	"io"
	"os"

	"hmj-agent/v2/internal/lint"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run checks the packages patterns name and returns the exit code: 0 when nothing is
// dropped, 1 when something is, and 2 when the packages cannot be loaded.
func run(patterns []string, stdout, stderr io.Writer) int {
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	found, err := lint.DroppedErrorsIn(".", patterns...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	for _, f := range found {
		fmt.Fprintln(stdout, f)
	}
	if len(found) > 0 {
		return 1
	}
	return 0
}
