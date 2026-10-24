package tools

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
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
			Name: "read_file",
			Description: "Read content from a file, each line prefixed with its number as `  12 | `. Without a limit, returns up to 2000 lines and at most 256KB per call; longer output ends with a trailer giving the offset to continue from. Optionally specify offset and limit to inspect specific line ranges (a limit is still capped at 256KB). " +
				"The line-number prefix is not part of the file: leave it out of edit_file's old_string. A directory's path returns its entries, as list_files with max_depth 3. " +
				"A picture (PNG, JPEG, GIF, WebP), such as a UI screenshot (e.g. from adb exec-out screencap -p), comes back as the image itself, scaled to at most 2000px a side; any other binary file as a one-line description with a command to look inside.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"path", "offset", "limit", "numbered"},
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file, or directory, to read: relative to the current working directory, or absolute (~ is expanded). Any path on the machine is allowed.",
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
						"description": "Whether to prefix each line with its number (default true). Set false for the file's exact text.",
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
	Numbered *bool  `json:"numbered"`
}

// numbered reports whether lines get their numbers: unless the call turns them off.
func (in readFileInput) numbered() bool { return in.Numbered == nil || *in.Numbered }

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
// the read was partial and with line numbers unless in turns them off.
func formatReadFileOutput(in readFileInput, offset int, selected []string, totalLines int) string {
	endIdx := min(offset+len(selected)-1, totalLines)

	var sb strings.Builder
	if in.Offset > 1 || (in.Limit > 0 && endIdx < totalLines) {
		sb.WriteString(fmt.Sprintf("--- %s (lines %d-%d of %d) ---\n", in.Path, offset, endIdx, totalLines))
	}

	for i, line := range selected {
		if in.numbered() {
			fmt.Fprintf(&sb, "%4d | ", offset+i)
		}
		sb.WriteString(line)
		if i < len(selected)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func (t *ReadFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	res, err := t.ExecuteWithImages(ctx, argsJSON)
	return res.Text, err
}

// ExecuteWithImages reads a file as Execute does, except that a picture (PNG, JPEG, GIF or
// WebP) comes back as the picture itself, scaled to fit maxImageSide.
func (t *ReadFileTool) ExecuteWithImages(ctx context.Context, argsJSON string) (Result, error) {
	in, err := parseReadFileArgs(argsJSON)
	if err != nil {
		return Result{}, err
	}

	absPath, err := ResolvePath(in.Path)
	if err != nil {
		return Result{}, err
	}
	if info, err := os.Stat(absPath); err == nil && info.IsDir() {
		text, err := readDirectory(ctx, in.Path)
		return Result{Text: text}, err
	}

	// A picture is shown, and any other binary file described rather than read, whatever its size.
	if info, err := os.Stat(absPath); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		head, err := readHead(absPath, binarySniffBytes)
		if err != nil {
			return Result{}, err
		}
		if desc, ok := describeBinary(in.Path, absPath, head, info.Size()); ok {
			kind, _ := detectBinary(head)
			if imageMediaTypes[kind.name] == "" {
				return Result{Text: desc}, nil
			}
			res, err := readImage(in.Path, absPath, kind.name, info.Size())
			if err != nil {
				return Result{Text: desc + " It cannot be shown: " + err.Error() + "."}, nil
			}
			return res, nil
		}
	}

	text, err := readTextFile(in, absPath)
	return Result{Text: text}, err
}

// readTextFile reads the lines in asks for from the text file at absPath.
func readTextFile(in readFileInput, absPath string) (string, error) {
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

	numberFrom := 0
	if in.numbered() {
		numberFrom = offset
	}
	kept, lineCut := capLinesToBytes(selected, MaxReadFileBytes, numberFrom)
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

// capLinesToBytes returns the longest prefix of lines whose text fits in limit bytes. When
// numberFrom is over 0 the lines are shown numbered from it, and their number prefixes count
// too. It always keeps one line: when that line alone is over the cap it is cut at a rune
// boundary and lineCut is set.
func capLinesToBytes(lines []string, limit, numberFrom int) (kept []string, lineCut bool) {
	size := 0
	for i, line := range lines {
		prefix := 0
		if numberFrom > 0 {
			prefix = lineNumberPrefixLen(numberFrom + i)
		}
		if size+prefix+len(line)+1 > limit {
			if i == 0 {
				// A first line of exactly limit bytes fits once its newline is not counted.
				cut := TruncateToBytes(line, limit-prefix)
				return []string{cut}, len(cut) < len(line)
			}
			return lines[:i], false
		}
		size += prefix + len(line) + 1
	}
	return lines, false
}

// lineNumberPrefixLen is the length of read_file's "%4d | " prefix for line n.
func lineNumberPrefixLen(n int) int {
	return max(len(strconv.Itoa(n)), 4) + 3
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

// readDirectoryDepth is the list_files max_depth read_file lists a directory it is given with:
// subdirectories two levels down, and their files.
const readDirectoryDepth = 3

// readDirectory answers a read_file of a directory with its entries, as list_files gives them.
func readDirectory(ctx context.Context, path string) (string, error) {
	args, err := json.Marshal(map[string]any{"path": path, "max_depth": readDirectoryDepth})
	if err != nil {
		return "", err
	}
	listing, err := (&ListFilesTool{}).Execute(ctx, string(args))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s is a directory; its entries, as list_files with max_depth %d lists them:\n%s", path, readDirectoryDepth, listing), nil
}
