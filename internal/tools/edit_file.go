package tools

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"hmj-agent/v2/internal/chat"
)

type EditFileTool struct{}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type:            "function",
		StreamArguments: true, // a large new_string takes long to write
		Function: chat.FunctionDefinition{
			Name: "edit_file",
			Description: "Make a surgical edit to an existing file by replacing old_string with new_string. old_string must match uniquely unless replace_all is true. " +
				"To make several changes to one file, pass them as edits instead of old_string and new_string: they apply in order, each to the text the previous ones left, and if any fails none is made.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"path", "old_string", "new_string", "edits", "replace_all", "ignore_whitespace"},
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to edit: relative to the current working directory, or absolute (~ is expanded). Any path on the machine is allowed.",
					},
					"old_string": map[string]any{
						"type":        "string",
						"description": "The exact existing text to be replaced, without read_file's line-number prefixes. Required unless edits is given.",
					},
					"new_string": map[string]any{
						"type":        "string",
						"description": "The new replacement text. Required unless edits is given.",
					},
					"edits": map[string]any{
						"type":        "array",
						"description": "Several replacements in this file, applied in order, all or none. Use instead of old_string and new_string.",
						"items": map[string]any{
							"type":             "object",
							chat.PropertyOrder: []string{"old_string", "new_string", "replace_all"},
							"properties": map[string]any{
								"old_string":  map[string]any{"type": "string", "description": "The exact existing text to be replaced, without read_file's line-number prefixes."},
								"new_string":  map[string]any{"type": "string", "description": "The new replacement text."},
								"replace_all": map[string]any{"type": "boolean", "description": "Optional flag to replace all occurrences instead of requiring a unique match."},
							},
							"required":             []string{"old_string", "new_string"},
							"additionalProperties": false,
						},
					},
					"replace_all": map[string]any{
						"type":        "boolean",
						"description": "Optional flag to replace all occurrences instead of requiring a unique match.",
					},
					"ignore_whitespace": map[string]any{
						"type":        "boolean",
						"description": "Optional flag to match old_string ignoring differences in leading/trailing whitespace, indentation, or line endings. Applies to every edit.",
					},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		},
	}
}

type editFileInput struct {
	Path             string     `json:"path"`
	OldString        string     `json:"old_string"`
	NewString        string     `json:"new_string"`
	ReplaceAll       bool       `json:"replace_all"`
	IgnoreWhitespace bool       `json:"ignore_whitespace"`
	Edits            []editSpec `json:"edits"`
}

// editSpec is one replacement of a multi-edit call.
type editSpec struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// steps returns the call's replacements, one or several, each as a single-edit input.
func (in editFileInput) steps() []editFileInput {
	if len(in.Edits) == 0 {
		return []editFileInput{in}
	}
	steps := make([]editFileInput, len(in.Edits))
	for i, e := range in.Edits {
		steps[i] = editFileInput{Path: in.Path, OldString: e.OldString, NewString: e.NewString, ReplaceAll: e.ReplaceAll, IgnoreWhitespace: in.IgnoreWhitespace}
	}
	return steps
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
	if len(in.Edits) > 0 {
		if in.OldString != "" || in.NewString != "" || in.ReplaceAll {
			return in, errors.New("give either old_string and new_string, or edits, not both")
		}
		for i, e := range in.Edits {
			if e.OldString == "" {
				return in, fmt.Errorf("edit %d of %d: old_string cannot be empty", i+1, len(in.Edits))
			}
		}
		return in, nil
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
	if hasReadFileLineNumbers(in.OldString) {
		return fmt.Errorf("old_string not found in %s: it starts each line with a number and \" | \", as read_file shows lines; those prefixes are not in the file, so leave them out of old_string and new_string", in.Path)
	}

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

// readFileLinePrefix matches the line-number prefix read_file puts before each line.
var readFileLinePrefix = regexp.MustCompile(`^\s*\d+ \| `)

// hasReadFileLineNumbers reports whether every line of s starts with read_file's line-number
// prefix, as when it is copied from read_file's output.
func hasReadFileLineNumbers(s string) bool {
	for l := range strings.SplitSeq(strings.TrimRight(s, "\r\n"), "\n") {
		if !readFileLinePrefix.MatchString(l) {
			return false
		}
	}
	return true
}

// applyEdits applies in's replacements to content in order, each to the text the previous ones
// left, and returns the new content, the number of replacements made and the 1-based numbers of
// the edits whose old_string and new_string are identical. Such an edit must still match, but
// changes nothing. When one fails it returns an error naming it, and nothing is changed.
func applyEdits(content string, in editFileInput) (string, int, []int, error) {
	steps := in.steps()
	total := 0
	var unchanged []int
	for i, step := range steps {
		next, count, err := applyEdit(content, step)
		if err != nil {
			if len(steps) > 1 {
				err = fmt.Errorf("edit %d of %d: %w; no edits were made", i+1, len(steps), err)
			}
			return "", 0, nil, err
		}
		if step.OldString == step.NewString {
			unchanged = append(unchanged, i+1)
			continue
		}
		content, total = next, total+count
	}
	return content, total, unchanged, nil
}

// loadEdit parses an edit_file call and reads its file. It reports a single edit that
// changes nothing as noChange.
func loadEdit(argsJSON string) (in editFileInput, absPath string, info os.FileInfo, data []byte, noChange bool, err error) {
	if in, err = parseEditFileArgs(argsJSON); err != nil {
		return
	}
	if len(in.Edits) == 0 && in.OldString == in.NewString {
		return in, "", nil, nil, true, nil
	}
	if absPath, err = ResolvePath(in.Path); err != nil {
		return
	}
	info, data, err = readEditableFile(absPath, in.Path)
	return
}

// PreviewEdit returns the text of the file an edit_file call names, before and after the call,
// worked out as the tool applies it but without writing anything. It is for showing the
// call's diff before it runs.
func PreviewEdit(argsJSON string) (before, after string, err error) {
	in, _, _, data, noChange, err := loadEdit(argsJSON)
	if err != nil || noChange {
		return "", "", cmp.Or(err, errors.New("no changes"))
	}
	after, _, _, err = applyEdits(string(data), in)
	return string(data), after, err
}

func (t *EditFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	in, absPath, info, data, noChange, err := loadEdit(argsJSON)
	if err != nil {
		return "", err
	}
	if noChange {
		return "no changes: old_string and new_string are identical", nil
	}

	newContent, count, unchanged, err := applyEdits(string(data), in)
	if err != nil {
		return "", err
	}
	// An edit that changes nothing is skipped with a note instead of failing the call, which
	// cost a session its other two edits.
	var unchangedNote string
	if len(unchanged) > 0 {
		if len(unchanged) == len(in.Edits) {
			return "no changes: every edit's old_string and new_string are identical", nil
		}
		unchangedNote = fmt.Sprintf("\n\n[note: %s of %d changed nothing, as old_string and new_string are identical]", numberedEdits(unchanged), len(in.Edits))
	}

	backupNote := backupFile(absPath, data)
	if err := os.WriteFile(absPath, []byte(newContent), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("failed to save %s: %w", in.Path, err)
	}

	msg := fmt.Sprintf("successfully replaced %d occurrence(s) in %s", count, in.Path) + backupNote
	if len(in.Edits) > 0 {
		msg = fmt.Sprintf("successfully made %d edits (%d replacement(s)) in %s", len(in.Edits)-len(unchanged), count, in.Path) + backupNote + unchangedNote
	}
	if warn := FastSyntaxCheck(ctx, absPath, []byte(newContent)); warn != "" {
		msg += "\n\n" + warn
	}
	return msg, nil
}

// numberedEdits names edits by number: "edit 2", "edits 1 and 3", "edits 1, 2 and 4".
func numberedEdits(nums []int) string {
	if len(nums) == 1 {
		return fmt.Sprintf("edit %d", nums[0])
	}
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return "edits " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
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
