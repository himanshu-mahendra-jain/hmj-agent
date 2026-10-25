package tools

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"hmj-agent/v2/internal/chat"
)

type WriteFileTool struct{}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type:            "function",
		StreamArguments: true, // a whole file can take tens of seconds to write
		Function: chat.FunctionDefinition{
			Name:        "write_file",
			Description: "Create a new file or overwrite an existing file with the provided content. Automatically creates parent directories.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"path", "content"},
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to create or overwrite: relative to the current working directory, or absolute (~ is expanded). Any path on the machine is allowed.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Full file content to write.",
					},
				},
				"required":             []string{"path", "content"},
				"additionalProperties": false,
			},
		},
	}
}

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var in writeFileInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if len(in.Content) > MaxWritableFileSize {
		return "", fmt.Errorf("content too large (>10MB): %d bytes", len(in.Content))
	}

	absPath, err := ResolvePath(in.Path)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	var backupNote string
	if info, err := os.Stat(absPath); err == nil && info.Mode().IsRegular() && info.Size() <= MaxWritableFileSize {
		if old, err := os.ReadFile(absPath); err == nil {
			backupNote = backupFile(absPath, old)
		}
	}
	if err := os.WriteFile(absPath, []byte(in.Content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write %s: %w", in.Path, err)
	}

	msg := fmt.Sprintf("successfully wrote %d bytes to %s", len(in.Content), in.Path) + backupNote
	if warn := FastSyntaxCheck(ctx, absPath, []byte(in.Content)); warn != "" {
		msg += "\n\n" + warn
	}
	return msg, nil
}
