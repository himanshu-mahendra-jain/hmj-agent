package tools

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// python3Path resolves the python3 interpreter once per process instead of
// scanning $PATH on every edited .py file.
var python3Path = sync.OnceValue(func() string {
	p, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	return p
})

// FastSyntaxCheck inspects file content for immediate syntax errors after write_file
// or edit_file. Returns an empty string if valid or unsupported, or a formatted
// diagnostic warning string if a syntax error is detected.
func FastSyntaxCheck(ctx context.Context, relPath string, content []byte) string {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".go":
		fset := token.NewFileSet()
		// parser.AllErrors reports all syntax issues found in the file.
		_, err := parser.ParseFile(fset, relPath, content, parser.AllErrors)
		if err != nil {
			// The error is a scanner.ErrorList, whose Error() names only the first entry.
			errLines := []string{err.Error()}
			if list, ok := errors.AsType[scanner.ErrorList](err); ok {
				errLines = errLines[:0]
				for _, e := range list {
					errLines = append(errLines, e.Error())
				}
			}
			if len(errLines) > 5 {
				errLines = append(errLines[:5], fmt.Sprintf("... and %d more error(s)", len(errLines)-5))
			}
			return fmt.Sprintf("[syntax warning: %s]", strings.Join(errLines, "\n"))
		}
		return ""

	case ".json":
		var v any
		if err := json.Unmarshal(content, &v); err != nil {
			return fmt.Sprintf("[syntax warning: invalid JSON in %s: %v]", relPath, err)
		}
		return ""

	case ".py":
		py := python3Path()
		if py == "" {
			return ""
		}
		checkCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()

		cmd := exec.CommandContext(checkCtx, py, "-c", "import ast, sys; ast.parse(sys.stdin.read())")
		cmd.Stdin = strings.NewReader(string(content))
		// Bound pipe drains after a context kill so a wedged interpreter
		// cannot hang the tool beyond the timeout.
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			if checkCtx.Err() != nil {
				return ""
			}
			msg := strings.TrimSpace(string(out))
			if msg != "" {
				// Keep only the first 2 lines of the python syntax error
				lines := strings.Split(msg, "\n")
				if len(lines) > 2 {
					msg = strings.Join(lines[:2], " ")
				}
				return fmt.Sprintf("[syntax warning: %s in %s]", msg, relPath)
			}
			return fmt.Sprintf("[syntax warning: invalid python syntax in %s]", relPath)
		}
		return ""

	default:
		return ""
	}
}
