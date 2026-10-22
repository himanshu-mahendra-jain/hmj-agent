package tools

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"hmj-agent/internal/chat"
)

type ReadFileTool struct{}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name:        "read_file",
			Description: "Read content from a file. Without a limit, returns up to 2000 lines and at most 256KB per call; longer output ends with a trailer giving the offset to continue from. Optionally specify offset and limit to inspect specific line ranges (a limit is still capped at 256KB).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to read: relative to the current working directory, or absolute (~ is expanded). Any path on the machine is allowed.",
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "Optional 1-based line number to start reading from.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Optional maximum number of lines to read.",
					},
					"numbered": map[string]any{
						"type":        "boolean",
						"description": "Optional boolean to include line numbers in output (default false).",
					},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		},
	}
}

type readFileInput struct {
	Path     string `json:"path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
	Numbered bool   `json:"numbered"`
}

// parseReadFileArgs decodes the tool arguments and requires a path.
func parseReadFileArgs(argsJSON string) (readFileInput, error) {
	var in readFileInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return in, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		return in, errors.New("path is required")
	}
	return in, nil
}

// statReadableFile checks that absPath is a regular file within MaxReadableFileSize.
// displayPath is used in error messages.
func statReadableFile(absPath, displayPath string) (os.FileInfo, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file", displayPath)
	}
	if info.Size() > MaxReadableFileSize {
		return nil, fmt.Errorf("file is too large (>10MB): %s", displayPath)
	}
	return info, nil
}

// readLineRange streams r line by line and returns the lines from the 1-based offset
// onward, up to limit lines when limit > 0, along with the total line count. Lines
// outside the range are only counted, never copied.
func readLineRange(r io.Reader, offset, limit int) ([]string, int) {
	br := bufio.NewReader(r)
	var (
		lineNum  int
		selected []string
		line     []byte // the selected line being assembled across buffer-sized pieces
		partial  bool   // a line longer than the buffer has been partly read
	)

	for {
		piece, readErr := br.ReadSlice('\n')
		inRange := lineNum+1 >= offset && (limit <= 0 || len(selected) < limit)
		if inRange {
			line = append(line, piece...)
		}
		if readErr == bufio.ErrBufferFull {
			partial = true
			continue // the line is longer than the buffer; read its next piece
		}
		if len(piece) > 0 || partial {
			partial = false
			lineNum++
			if inRange {
				selected = append(selected, strings.TrimRight(string(line), "\r\n"))
				line = line[:0]
			}
		}
		if readErr != nil {
			break
		}
	}
	return selected, lineNum
}

// formatReadFileOutput renders the selected lines, prefixed with a range header when
// the read was partial and with line numbers when in.Numbered is set.
func formatReadFileOutput(in readFileInput, offset int, selected []string, totalLines int) string {
	endIdx := min(offset+len(selected)-1, totalLines)

	var sb strings.Builder
	if in.Offset > 1 || (in.Limit > 0 && endIdx < totalLines) {
		sb.WriteString(fmt.Sprintf("--- %s (lines %d-%d of %d) ---\n", in.Path, offset, endIdx, totalLines))
	}

	for i, line := range selected {
		curNum := offset + i
		if in.Numbered {
			sb.WriteString(fmt.Sprintf("%4d | %s\n", curNum, line))
		} else {
			sb.WriteString(line)
			if i < len(selected)-1 {
				sb.WriteString("\n")
			}
		}
	}

	return sb.String()
}

func (t *ReadFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	in, err := parseReadFileArgs(argsJSON)
	if err != nil {
		return "", err
	}

	absPath, err := ResolvePath(in.Path)
	if err != nil {
		return "", err
	}

	info, err := statReadableFile(absPath, in.Path)
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "[empty file]", nil
	}

	file, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	offset := in.Offset
	if offset <= 0 {
		offset = 1
	}

	// Without a limit the read is capped at MaxReadFileLines; an explicit limit is honoured
	// up to the same byte cap. Lines past the selection are still counted for the trailer.
	requested := in.Limit
	if requested <= 0 {
		requested = MaxReadFileLines
	}
	selected, totalLines := readLineRange(file, offset, requested)

	if offset > totalLines {
		return fmt.Sprintf("file has %d lines (requested offset %d)", totalLines, offset), nil
	}

	kept, lineCut := capLinesToBytes(selected, MaxReadFileBytes)
	out := formatReadFileOutput(in, offset, kept, totalLines)

	lastShown := offset + len(kept) - 1
	switch {
	case lineCut:
		note := fmt.Sprintf("line %d is longer than %d bytes and was cut", offset, MaxReadFileBytes)
		if lastShown < totalLines {
			note += fmt.Sprintf("; call read_file again with offset=%d to continue", lastShown+1)
		}
		out += fmt.Sprintf("\n--- truncated: %s ---", note)
	case lastShown < totalLines && (in.Limit <= 0 || len(kept) < len(selected)):
		out += fmt.Sprintf("\n--- truncated: showing lines %d-%d of %d; call read_file again with offset=%d ---", offset, lastShown, totalLines, lastShown+1)
	}
	return out, nil
}

// capLinesToBytes returns the longest prefix of lines whose text fits in limit bytes. It always
// keeps one line: when that line alone is over the cap it is cut at a rune boundary and lineCut is set.
func capLinesToBytes(lines []string, limit int) (kept []string, lineCut bool) {
	size := 0
	for i, line := range lines {
		if size+len(line)+1 > limit {
			if i == 0 {
				// A first line of exactly limit bytes fits once its newline is not counted.
				cut := TruncateToBytes(line, limit)
				return []string{cut}, len(cut) < len(line)
			}
			return lines[:i], false
		}
		size += len(line) + 1
	}
	return lines, false
}

// TruncateToBytes returns the longest prefix of s that is at most n bytes and ends on a rune boundary.
func TruncateToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
