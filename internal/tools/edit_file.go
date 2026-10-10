package tools

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"hmj-agent/internal/chat"
)

type EditFileTool struct{}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name:        "edit_file",
			Description: "Make a surgical edit to an existing file by replacing old_string with new_string. old_string must match uniquely unless replace_all is true.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to edit: relative to the current working directory, or absolute (~ is expanded). Any path on the machine is allowed.",
					},
					"old_string": map[string]any{
						"type":        "string",
						"description": "The exact existing text to be replaced.",
					},
					"new_string": map[string]any{
						"type":        "string",
						"description": "The new replacement text.",
					},
					"replace_all": map[string]any{
						"type":        "boolean",
						"description": "Optional flag to replace all occurrences instead of requiring a unique match.",
					},
					"ignore_whitespace": map[string]any{
						"type":        "boolean",
						"description": "Optional flag to match old_string ignoring differences in leading/trailing whitespace, indentation, or line endings.",
					},
				},
				"required":             []string{"path", "old_string", "new_string"},
				"additionalProperties": false,
			},
		},
	}
}

type editFileInput struct {
	Path             string `json:"path"`
	OldString        string `json:"old_string"`
	NewString        string `json:"new_string"`
	ReplaceAll       bool   `json:"replace_all"`
	IgnoreWhitespace bool   `json:"ignore_whitespace"`
}

// maxNormalizeHintBytes bounds the whole-file whitespace-normalization hint
// built on the failed-edit path; beyond this the generic error is returned.
const maxNormalizeHintBytes = 256 * 1024

type lineMatch struct {
	startLine int // 1-indexed
	endLine   int // 1-indexed
	startByte int
	endByte   int
}

func findTrimmedLineMatches(content, oldStr string) ([]lineMatch, []string) {
	if content == "" || oldStr == "" {
		return nil, nil
	}
	contentLines := strings.Split(content, "\n")
	targetLines := strings.Split(strings.TrimRight(oldStr, "\r\n"), "\n")
	if len(targetLines) == 0 || len(targetLines) > len(contentLines) {
		return nil, contentLines
	}

	targetTrimmed := make([]string, len(targetLines))
	for i, l := range targetLines {
		targetTrimmed[i] = strings.TrimSpace(strings.TrimRight(l, "\r"))
	}

	// Calculate line byte offsets in content
	lineOffsets := make([]int, len(contentLines))
	offset := 0
	for i, line := range contentLines {
		lineOffsets[i] = offset
		offset += len(line) + 1 // +1 for '\n'
	}

	var matches []lineMatch
	for i := 0; i <= len(contentLines)-len(targetTrimmed); i++ {
		window := contentLines[i : i+len(targetTrimmed)]
		matched := slices.EqualFunc(window, targetTrimmed, func(c, t string) bool {
			return strings.TrimSpace(strings.TrimRight(c, "\r")) == t
		})
		if matched {
			startByte := lineOffsets[i]
			endLineIdx := i + len(targetTrimmed) - 1
			endByte := lineOffsets[endLineIdx] + len(contentLines[endLineIdx])
			matches = append(matches, lineMatch{
				startLine: i + 1,
				endLine:   endLineIdx + 1,
				startByte: startByte,
				endByte:   endByte,
			})
			i += len(targetTrimmed) - 1 // advance past match to prevent overlapping windows
		}
	}
	return matches, contentLines
}

// parseEditFileArgs decodes the tool arguments and checks the required fields.
func parseEditFileArgs(argsJSON string) (editFileInput, error) {
	var in editFileInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return in, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		return in, errors.New("path is required")
	}
	if in.OldString == "" {
		return in, errors.New("old_string cannot be empty")
	}
	return in, nil
}

// readEditableFile stats and reads the file at absPath, rejecting directories and
// files larger than MaxEditableFileSize. displayPath is used in error messages.
func readEditableFile(absPath, displayPath string) (os.FileInfo, []byte, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("%s is a directory, not a file", displayPath)
	}
	if info.Size() > MaxEditableFileSize {
		return nil, nil, fmt.Errorf("file is too large (>10MB): %s", displayPath)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxEditableFileSize {
		return nil, nil, fmt.Errorf("file is too large (>10MB): %s", displayPath)
	}
	return info, data, nil
}

// applyEdit applies in to content and returns the new content and the number of
// replacements made. It falls back to whitespace-tolerant matching when the exact
// match fails, and returns a descriptive error when no usable match exists.
func applyEdit(content string, in editFileInput) (string, int, error) {
	if count := strings.Count(content, in.OldString); count > 0 {
		return replaceExactMatches(content, in, count)
	}

	// Exact match failed. Check flexible line-trimmed matches.
	matches, contentLines := findTrimmedLineMatches(content, in.OldString)
	if len(matches) == 0 {
		return "", 0, notFoundEditError(content, contentLines, in)
	}
	if !in.IgnoreWhitespace {
		// Provide detailed line numbers and preview in error
		return "", 0, nearMatchEditError(in, matches[0], contentLines)
	}
	return replaceFlexibleMatches(content, in, matches)
}

// replaceExactMatches replaces count exact occurrences of in.OldString, requiring a
// unique match unless in.ReplaceAll is set.
func replaceExactMatches(content string, in editFileInput, count int) (string, int, error) {
	if count > 1 && !in.ReplaceAll {
		return "", 0, fmt.Errorf("old_string found %d times in %s; provide more surrounding context to match uniquely, or set replace_all to true", count, in.Path)
	}
	n := 1
	if in.ReplaceAll {
		n = -1
	}
	return strings.Replace(content, in.OldString, in.NewString, n), count, nil
}

// replaceFlexibleMatches replaces line-trimmed matches of in.OldString, requiring a
// unique match unless in.ReplaceAll is set.
func replaceFlexibleMatches(content string, in editFileInput, matches []lineMatch) (string, int, error) {
	if len(matches) > 1 && !in.ReplaceAll {
		return "", 0, fmt.Errorf("old_string found %d times with flexible whitespace in %s; provide more surrounding context to match uniquely, or set replace_all to true", len(matches), in.Path)
	}
	count := len(matches)
	if !in.ReplaceAll {
		count = 1
		matches = matches[:1]
	}
	newContent := content
	// Replace backwards so earlier byte offsets remain valid. findTrimmedLineMatches
	// returns ascending, non-overlapping matches within content, so each splice only
	// changes bytes after the next (earlier) match's endByte. The bounds check below
	// guards that invariant rather than any expected input.
	for _, m := range slices.Backward(matches) {
		if m.startByte > len(newContent) || m.endByte > len(newContent) || m.startByte > m.endByte {
			return "", 0, fmt.Errorf("internal error: replacement slice bounds invalid [%d:%d] in %s", m.startByte, m.endByte, in.Path)
		}
		newContent = newContent[:m.startByte] + in.NewString + newContent[m.endByte:]
	}
	return newContent, count, nil
}

// nearMatchEditError reports a near match that differs only in whitespace, quoting
// the matched lines (truncated to 80 runes each).
func nearMatchEditError(in editFileInput, m lineMatch, contentLines []string) error {
	var preview strings.Builder
	for idx := m.startLine; idx <= m.endLine; idx++ {
		lineText := contentLines[idx-1]
		runes := []rune(lineText)
		if len(runes) > 80 {
			lineText = string(runes[:77]) + "..."
		}
		fmt.Fprintf(&preview, "  line %d: %s\n", idx, lineText)
	}
	return fmt.Errorf("old_string not found in %s: a near match exists with different whitespace or indentation at line(s) %d-%d:\n%sRe-read the file around the target with read_file to capture exact tabs/spaces, or retry with ignore_whitespace: true", in.Path, m.startLine, m.endLine, preview.String())
}

// notFoundEditError builds the error for an edit whose old_string matches nowhere,
// adding whitespace or first-line hints when they apply.
func notFoundEditError(content string, contentLines []string, in editFileInput) error {
	// Token-based whitespace normalization hint. Bounded to smaller
	// files so a failed edit against a huge file cannot build a full
	// second copy of the content just to produce an error message.
	if len(content) <= maxNormalizeHintBytes {
		normContent := normalizeWhitespace(content)
		normOld := normalizeWhitespace(in.OldString)
		if normOld != "" && strings.Contains(normContent, normOld) {
			return fmt.Errorf("old_string not found in %s: a near match exists with different whitespace or indentation; re-read the file around the target with read_file to capture exact tabs/spaces, or use a smaller 3-5 line unique anchor, or retry with ignore_whitespace: true", in.Path)
		}
	}

	// Helpful diagnostic: did line 1 match anywhere?
	targetLines := strings.Split(strings.TrimRight(in.OldString, "\r\n"), "\n")
	firstTrim := strings.TrimSpace(targetLines[0])
	if firstTrim != "" && len(contentLines) > 0 {
		for idx, cl := range contentLines {
			if strings.TrimSpace(cl) == firstTrim {
				return fmt.Errorf("old_string not found in %s: line 1 matches line %d, but subsequent lines differ. Inspect lines %d-%d with read_file", in.Path, idx+1, max(1, idx-1), min(len(contentLines), idx+len(targetLines)+2))
			}
		}
	}

	return fmt.Errorf("old_string not found in %s", in.Path)
}

func (t *EditFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	in, err := parseEditFileArgs(argsJSON)
	if err != nil {
		return "", err
	}
	if in.OldString == in.NewString {
		return "no changes: old_string and new_string are identical", nil
	}

	absPath, err := ResolvePath(in.Path)
	if err != nil {
		return "", err
	}

	info, data, err := readEditableFile(absPath, in.Path)
	if err != nil {
		return "", err
	}

	newContent, count, err := applyEdit(string(data), in)
	if err != nil {
		return "", err
	}

	backupNote := backupFile(absPath, data)
	if err := os.WriteFile(absPath, []byte(newContent), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("failed to save %s: %w", in.Path, err)
	}

	msg := fmt.Sprintf("successfully replaced %d occurrence(s) in %s", count, in.Path) + backupNote
	if warn := FastSyntaxCheck(ctx, absPath, []byte(newContent)); warn != "" {
		msg += "\n\n" + warn
	}
	return msg, nil
}

func normalizeWhitespace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
