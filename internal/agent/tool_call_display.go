package agent

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"hmj-agent/internal/chat"
	"hmj-agent/internal/diff"
	"hmj-agent/internal/session"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
	"hmj-agent/internal/tools"
	"os"
	"strings"
	"time"
)

// PrintConversationHistory prints the transcript of a resumed conversation. A compaction is shown
// as a notice, followed by its summary when the provider's summary can be read.
func PrintConversationHistory(conv *session.Conversation) {
	calls := make(map[string]chat.ToolCall)
	for _, m := range conv.Messages {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = tc
		}
	}

	var printedAny bool
	var lastRole string
	for _, m := range conv.Messages {
		switch m.Role {
		case "system":
			continue
		case "user":
			if lastRole != "" && lastRole != "user" {
				fmt.Println()
			}
			fmt.Printf("%s %s %s\n", theme.Prompt(">"), theme.User("you:"), theme.Text(theme.Sanitize(m.Content)))
			lastRole = "user"
			printedAny = true
		case "assistant":
			if b := m.CompactionBlock(); b != nil {
				fmt.Println(theme.Muted("[the earlier conversation was compacted]"))
				if summary := b.ReadableSummary(); summary != "" {
					fmt.Printf("\n%s\n", theme.Muted(theme.Sanitize(summary)))
				}
				lastRole = "assistant"
				printedAny = true
				continue
			}
			for _, tc := range m.ToolCalls {
				switch {
				case quietTools[tc.Function.Name]: // a quiet call is printed with its result
				case tc.Function.Name == "write_file":
					// The file has been written since, so a diff against it would be wrong.
					fmt.Printf("\n%s\n", toolCallLine(tc.Function.Name, tc.Function.Arguments))
				default:
					printExpandedToolCall(tc.Function.Name, tc.Function.Arguments)
				}
			}
			if m.Content != "" {
				fmt.Printf("\n%s\n", terminal.RenderMarkdown(theme.Sanitize(m.Content)))
			}
			lastRole = "assistant"
			printedAny = true
		case "tool":
			tc := calls[m.ToolCallID]
			if quietTools[tc.Function.Name] {
				printQuietToolResult(tc.Function.Name, tc.Function.Arguments, m.Content, len(m.Images) > 0, 0)
			} else {
				printExpandedToolResult(tc.Function.Name, m.Content)
			}
			lastRole = "tool"
			printedAny = true
		}
	}
	if printedAny {
		fmt.Println()
	}
}

func printExpandedToolCall(name, argsJSON string) {
	if name == "edit_file" {
		var raw map[string]any
		if json.Unmarshal([]byte(argsJSON), &raw) == nil {
			get := func(key string) string { s, _ := raw[key].(string); return s }
			edits, _ := raw["edits"].([]any)
			printEditCall(argsJSON, get("path"), get("old_string"), get("new_string"), edits)
			return
		}
	}
	fmt.Printf("\n%s\n", toolCallLine(name, argsJSON))
	if name == "write_file" {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(argsJSON), &in) == nil && in.Path != "" {
			printWriteFileDiff(in.Path, in.Content)
		}
	}
}

// printWriteFileDiff prints what a write_file call is about to change, before it runs: the
// file's content against the new content, or every line as added for a new file. A file that
// cannot be read, or is too large for write_file to back up, gets no diff.
func printWriteFileDiff(path, content string) {
	absPath, err := tools.ResolvePath(path)
	if err != nil {
		return
	}
	var d *diff.File
	info, err := os.Stat(absPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		d = diff.Text(path, "", EnsureTrailingNewline(content), diff.Options{Inline: false, MaxLines: maxDiffRows})
	case err != nil || !info.Mode().IsRegular() || info.Size() > tools.MaxWritableFileSize:
		return
	default:
		old, err := os.ReadFile(absPath)
		if err != nil {
			return
		}
		d = diff.Text(path, string(old), content, diff.Options{Inline: true, MaxLines: maxDiffRows})
	}
	if d.Empty() {
		fmt.Printf("  %s\n", theme.Muted("[no change: the file already has this content]"))
		return
	}
	printDiff(d)
}

// toolCallLine is the transcript's line for a tool call: the tool's badge and the arguments
// that say what it does.
func toolCallLine(name, argsJSON string) string {
	// Every displayed field comes from the model, so escape sequences are stripped
	// before printing: a command must not be able to redraw itself as something else.
	badge := theme.ToolBadge(theme.Sanitize(name))
	var raw map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		return fmt.Sprintf("%s %s %s", badge, theme.Muted("(unparseable arguments)"), theme.Muted(theme.Sanitize(argsJSON)))
	}

	// Display-only extraction: missing or non-string argument fields render as
	// empty strings in the transcript; the actual tool execution re-parses args
	// strictly and reports real errors.
	get := func(key string) string {
		s, ok := raw[key].(string)
		if !ok {
			return ""
		}
		return s
	}
	show := func(key string) string { return theme.Sanitize(get(key)) }

	switch name {
	case "read_file":
		path := show("path")
		if off, hasOff := raw["offset"]; hasOff {
			return fmt.Sprintf("%s %s %s", badge, theme.FilePath(path), theme.Muted(fmt.Sprintf("(offset: %s)", theme.Sanitize(fmt.Sprint(off)))))
		}
		return fmt.Sprintf("%s %s", badge, theme.FilePath(path))

	case "write_file":
		path, content := show("path"), get("content")
		return fmt.Sprintf("%s %s %s", badge, theme.FilePath(path), theme.Muted(fmt.Sprintf("(%d bytes)", len(content))))

	case "list_files":
		path := show("path")
		if path == "" {
			path = "."
		}
		if p := show("pattern"); p != "" {
			return fmt.Sprintf("%s %s %s", badge, theme.FilePath(path), theme.Muted("(pattern: "+p+")"))
		}
		return fmt.Sprintf("%s %s", badge, theme.FilePath(path))

	case "grep":
		var where []string
		for _, key := range []string{"path", "glob", "output_mode"} {
			if v := show(key); v != "" {
				where = append(where, key+": "+v)
			}
		}
		if len(where) > 0 {
			return fmt.Sprintf("%s %s %s", badge, theme.Info(show("pattern")), theme.Muted("("+strings.Join(where, ", ")+")"))
		}
		return fmt.Sprintf("%s %s", badge, theme.Info(show("pattern")))

	case "run_command":
		head, more := commandHead(show("command"))
		if raw["background"] == true {
			head += theme.Muted(" (background)")
		}
		if more > 0 {
			return fmt.Sprintf("%s %s\n%s", badge, theme.Info(head), theme.Muted(theme.ASCII("… ")+plural(more, "more line")))
		}
		return fmt.Sprintf("%s %s", badge, theme.Info(head))

	case "check_command":
		what := "job " + theme.Sanitize(fmt.Sprint(raw["job"]))
		if raw["stop"] == true {
			what += " (stop)"
		} else if w, ok := raw["wait_seconds"].(float64); ok && w > 0 {
			what += fmt.Sprintf(" (wait up to %ds)", int(w))
		}
		return fmt.Sprintf("%s %s", badge, theme.Info(what))

	case "web_search":
		var filters []string
		for _, f := range []struct{ key, label string }{{"allowed_domains", "only"}, {"blocked_domains", "not"}} {
			if list, ok := raw[f.key].([]any); ok && len(list) > 0 {
				names := make([]string, len(list))
				for i, d := range list {
					names[i] = theme.Sanitize(fmt.Sprint(d))
				}
				filters = append(filters, f.label+": "+strings.Join(names, ", "))
			}
		}
		if r := show("recency"); r != "" {
			filters = append(filters, "past "+r)
		}
		if len(filters) > 0 {
			return fmt.Sprintf("%s %s %s", badge, theme.Info(show("query")), theme.Muted("("+strings.Join(filters, "; ")+")"))
		}
		return fmt.Sprintf("%s %s", badge, theme.Info(show("query")))

	case "fetch_url":
		return fmt.Sprintf("%s %s", badge, theme.Info(show("url")))

	default:
		return fmt.Sprintf("%s %s", badge, theme.Muted(theme.Sanitize(argsJSON)))
	}
}

// printEditFileDiff prints an edit_file call's change as a line-numbered diff with word-level
// highlighting. When the old text cannot be located in the file (or the call is a creation),
// it falls back to simple minus/plus blocks so the user always sees what will change.
func printEditFileDiff(path, oldStr, newStr string) {
	if oldStr == "" && newStr == "" {
		return
	}

	oldText, haveOld := readFileForDiff(path)
	var d *diff.File
	if haveOld && oldStr != "" && strings.Contains(oldText, oldStr) {
		newText := strings.Replace(oldText, oldStr, newStr, 1)
		d = diff.Text(path, oldText, newText, diff.Options{Inline: true, MaxLines: maxDiffRows})
	} else if !haveOld && oldStr == "" {
		// New file creation: show every line as an addition.
		d = diff.Text(path, "", EnsureTrailingNewline(newStr), diff.Options{Inline: false, MaxLines: maxDiffRows})
	}

	if d != nil && !d.Empty() {
		printDiff(d)
		return
	}

	// Fallback: naive minus/plus block (unreadable path, or not-found old_string).
	printEditBlocks(oldStr, newStr)
}

// printEditCall renders an edit_file call as one diff of the whole call, worked out as the
// tool will apply it, so replace_all, ignore_whitespace and every one of several edits show.
// When that fails (the tool will report why), it falls back to printEditFileDiff for a
// single edit, or minus/plus blocks for each of several.
func printEditCall(argsJSON, path, oldStr, newStr string, edits []any) {
	if len(edits) == 0 {
		fmt.Printf("\n%s %s\n", theme.ToolBadge("edit_file"), theme.FilePath(theme.Sanitize(path)))
	} else {
		fmt.Printf("\n%s %s %s\n", theme.ToolBadge("edit_file"), theme.FilePath(theme.Sanitize(path)), theme.Muted(fmt.Sprintf("(%d edits)", len(edits))))
	}
	if before, after, err := tools.PreviewEdit(argsJSON); err == nil {
		if d := diff.Text(path, before, after, diff.Options{Inline: true, MaxLines: maxDiffRows}); !d.Empty() {
			printDiff(d)
			return
		}
	}
	if len(edits) == 0 {
		printEditFileDiff(path, oldStr, newStr)
		return
	}
	for _, e := range edits {
		m, _ := e.(map[string]any)
		oldStr, _ := m["old_string"].(string)
		newStr, _ := m["new_string"].(string)
		printEditBlocks(oldStr, newStr)
	}
}

// printEditBlocks shows a replacement as the old lines in minus blocks and the new lines in
// plus blocks.
func printEditBlocks(oldStr, newStr string) {
	if oldStr != "" {
		for l := range strings.SplitSeq(strings.TrimRight(oldStr, "\n"), "\n") {
			fmt.Printf("  %s %s\n", theme.DelGlyph(), theme.RemovedBlock(" "+theme.Sanitize(l)+" "))
		}
	}
	if newStr != "" {
		for l := range strings.SplitSeq(strings.TrimRight(newStr, "\n"), "\n") {
			fmt.Printf("  %s %s\n", theme.AddGlyph(), theme.Added(theme.Sanitize(l)))
		}
	}
}

// maxDiffRows caps how many diff rows one tool call prints inline.
const maxDiffRows = 120

// readFileForDiff reads a file for diffing, returning ok=false when it does not
// exist or cannot be read (never an error: display must stay best-effort).
func readFileForDiff(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	absPath, err := tools.ResolvePath(path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// EnsureTrailingNewline appends a newline to s unless s is empty or already
// ends with one. Exported for testing.
func EnsureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// printDiff renders a computed diff with the theme palette.
func printDiff(d *diff.File) {
	paint := func(s string, kind diff.SegmentStyle) string {
		// Diff text is file content or model output: strip escape sequences so a
		// line cannot drive the terminal (cursor moves, OSC 52 clipboard writes).
		s = theme.Sanitize(s)
		switch kind {
		case diff.StyleLineNum:
			return theme.Faint(s)
		case diff.StyleGlyphAdd:
			return theme.AddGlyph()
		case diff.StyleGlyphDel:
			return theme.DelGlyph()
		case diff.StyleTextAdd:
			return s
		case diff.StyleMarkAdd:
			return theme.Mark(s)
		case diff.StyleTextDel:
			return s
		case diff.StyleMarkDel:
			return theme.Mark(s)
		case diff.StyleRule, diff.StyleFaint:
			return theme.Faint(s)
		case diff.StyleAccent:
			return theme.Accent(s)
		case diff.StylePath:
			return theme.FilePath(s)
		default:
			// Context lines: pure white text
			return theme.BrightWhite(s)
		}
	}

	// Single clean diff header with 2-space indent (DiffHeader gates the
	// separator glyph for accessibility mode).
	fmt.Printf("  %s\n", theme.DiffHeader(theme.Sanitize(d.Path), theme.FormatDiffStats(d.Added, d.Removed)))

	for i, row := range d.Render(paint) {
		if i == 0 {
			// Skip the redundant header row emitted by d.Render
			continue
		}
		if strings.Contains(row.Raw, "@@") {
			continue
		}
		if row.Kind == diff.Del {
			fmt.Printf("  %s\n", theme.RemovedLine(row.Number, row.Glyph, row.Body))
		} else if row.Kind == diff.Add {
			fmt.Printf("  %s\n", theme.AddedLine(row.Number, row.Glyph, row.Body))
		} else {
			if row.Notice && row.Body == "" {
				// Hunk separator: its "···" lives in Raw, not Body.
				fmt.Printf("  %s\n", theme.Faint(theme.ASCII(row.Raw)))
				continue
			}
			cleanNum := strings.TrimRight(theme.StripANSI(row.Number), " ")
			cleanBody := strings.TrimRight(theme.StripANSI(row.Body), " \t\r\n")
			if row.Notice {
				cleanBody = theme.ASCII(cleanBody)
			}
			fmt.Printf("  %s   %s\n", theme.Faint(cleanNum), theme.BrightWhite(cleanBody))
		}
	}
}

func printExpandedToolResult(toolName string, output string, duration ...time.Duration) {
	output = strings.TrimRight(output, "\r\n")
	var durStr string
	if len(duration) > 0 && duration[0] > 0 {
		durStr = theme.Muted(fmt.Sprintf("(%s)", FormatDuration(duration[0])))
	}

	if output == "" {
		if durStr != "" {
			fmt.Printf("  %s\n", durStr)
		}
		return
	}

	if strings.HasPrefix(output, "error:") {
		fmt.Fprintf(os.Stderr, "  %s\n", theme.Danger(theme.Sanitize(output)))
		if durStr != "" {
			fmt.Printf("  %s\n", durStr)
		}
		return
	}

	hidden := 0
	if toolName == "run_command" || toolName == "check_command" {
		output, hidden = commandTail(output)
	}
	if theme.Accessibility() {
		if hidden > 0 {
			output = fmt.Sprintf("... %s hidden\n%s", plural(hidden, "line"), output)
		}
		fmt.Println(resultBlock(toolName, output))
	} else {
		if hidden > 0 {
			fmt.Printf("  %s %s\n", theme.Faint("│"), theme.Muted("… "+plural(hidden, "line")+" hidden"))
		}
		for l := range strings.SplitSeq(output, "\n") {
			fmt.Printf("  %s %s\n", theme.Faint("│"), theme.Sanitize(l))
		}
	}
	if durStr != "" {
		fmt.Printf("  %s\n", durStr)
	}
}

// resultBlock frames a tool result for accessibility mode: a [RESULT name] header, the
// output indented by two spaces with no gutter, and an [END RESULT] footer. Without a name
// the header is [OUTPUT].
func resultBlock(name, output string) string {
	header := "[OUTPUT]"
	if name != "" {
		header = "[RESULT " + name + "]"
	}
	var sb strings.Builder
	sb.WriteString(header + "\n")
	for l := range strings.SplitSeq(output, "\n") {
		sb.WriteString("  " + theme.Sanitize(l) + "\n")
	}
	sb.WriteString("[END RESULT]")
	return sb.String()
}
