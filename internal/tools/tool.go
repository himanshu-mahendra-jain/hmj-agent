package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"hmj-agent/internal/chat"
)

// Limits applied by the built-in tools.
const (
	// MaxReadableFileSize is the largest file read_file will load.
	MaxReadableFileSize = 10 * 1024 * 1024
	// MaxEditableFileSize is the largest file edit_file will load and rewrite.
	MaxEditableFileSize = 10 * 1024 * 1024
	// MaxWritableFileSize is the largest content write_file will accept.
	MaxWritableFileSize = 10 * 1024 * 1024
	// MaxReadFileLines is how many lines read_file returns when the call sets no limit.
	MaxReadFileLines = 2000
	// MaxReadFileBytes caps the text read_file returns in one call, whether or not a limit is given.
	MaxReadFileBytes = 256 * 1024
	// MaxCommandOutputBytes is the combined stdout and stderr kept from run_command.
	MaxCommandOutputBytes = 64 * 1024
	// MaxFetchBodyBytes is the largest response body fetch_url will read.
	MaxFetchBodyBytes = 4 * 1024 * 1024
)

// User-Agent headers. Web pages get a generic browser string that does not single out
// this tool; the search APIs get an identifying one, which Wikipedia's API policy requires.
const (
	browserUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
	apiUserAgent     = "HMJ-Agent/1.0 (terminal coding agent)"
)

// Tool is a capability offered to the model, such as reading a file or running a shell command.
type Tool interface {
	// Name returns the identifier the model uses to call the tool.
	Name() string
	// Definition returns the schema advertised to the model.
	Definition() chat.ToolDefinition
	// Execute runs the tool with the JSON-encoded arguments in argsJSON and returns the text result
	// shown to the model.
	Execute(ctx context.Context, argsJSON string) (string, error)
}

// DefaultTools returns a new instance of each built-in tool.
func DefaultTools() []Tool {
	return []Tool{
		&ReadFileTool{},
		&EditFileTool{},
		&WriteFileTool{},
		&ListFilesTool{},
		&RunCommandTool{},
		&WebSearchTool{},
		&FetchURLTool{},
	}
}

// ResolvePath turns a user/model-supplied path into an absolute, cleaned path.
// Relative paths resolve against the current working directory; a leading "~"
// expands to the user's home directory. No confinement is applied: the agent
// is intended to run on a dedicated machine with full filesystem access.
func ResolvePath(userPath string) (string, error) {
	p := strings.TrimSpace(userPath)
	if p == "" {
		p = "."
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current working directory: %w", err)
		}
		p = filepath.Join(wd, p)
	}
	return filepath.Clean(p), nil
}
