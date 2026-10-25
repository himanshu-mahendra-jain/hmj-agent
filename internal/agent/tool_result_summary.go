package agent

import (
	"encoding/json/v2"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hmj-agent/v2/internal/session"
	"hmj-agent/v2/internal/terminal/theme"
)

// quietTools are the tools whose output only the model needs: the transcript shows each call
// once it has finished, on one line with a summary of what it found, instead of the output.
var quietTools = map[string]bool{"read_file": true, "list_files": true, "grep": true, "fetch_url": true, "web_search": true}

// commandTailLines is how many of run_command's last output lines the transcript shows.
// Builds and test runners print their failures and totals last.
const commandTailLines = 10

// printQuietToolResult prints a quiet tool's call with a summary of its output and the time it
// took, and for web_search a line per result. An error is shown in full under the call.
func printQuietToolResult(name, argsJSON, output string, hasImages bool, duration time.Duration) {
	line := toolCallLine(name, argsJSON)
	output = strings.TrimRight(output, "\r\n")
	if strings.HasPrefix(output, "error:") {
		fmt.Printf("\n%s\n", line)
		printExpandedToolResult(name, output, duration)
		return
	}
	summary, details := summarizeToolResult(name, argsJSON, output, hasImages)
	if replacedResult(output) {
		summary, details = output, nil // a summary of it would read as the tool's own output
	}
	sep := theme.Faint(theme.ASCII(" · "))
	line += sep + theme.Muted(theme.Sanitize(summary))
	if duration > 0 {
		line += sep + theme.Muted(FormatDuration(duration))
	}
	fmt.Printf("\n%s\n", line)
	for _, d := range details {
		fmt.Printf("  %s %s\n", theme.Faint(theme.ASCII("│")), theme.Muted(theme.Sanitize(d)))
	}
}

// removedResultNote matches the note that replaces a result removed for being over the model's
// context limit (see removeLargestStepResult).
var removedResultNote = regexp.MustCompile(`^\[the \S+ result \(\d+ KB\) was removed: `)

// replacedResult reports whether output is not the tool's but hmj's note in its place: the call
// was cancelled before it ran, or its result was removed.
func replacedResult(output string) bool {
	return output == session.CancelledToolResult || removedResultNote.MatchString(output)
}

// summarizeToolResult sums up a quiet tool's output in a few words, with any lines to show
// under the call.
func summarizeToolResult(name, argsJSON, output string, hasImages bool) (string, []string) {
	switch name {
	case "read_file":
		return summarizeRead(output, hasImages), nil
	case "list_files":
		return summarizeList(output), nil
	case "grep":
		return summarizeGrep(argsJSON, output), nil
	case "fetch_url":
		return summarizeFetch(output), nil
	case "web_search":
		return summarizeSearch(output)
	}
	return countLines(output), nil
}

// readLineNumber matches the number prefix read_file puts on each line.
var readLineNumber = regexp.MustCompile(`^ *(\d+) \| `)

func summarizeRead(output string, hasImages bool) string {
	if hasImages {
		// "path: PNG image, 1080×2400, 72 KB; shown scaled to ...". The call shows the path.
		_, about, _ := strings.Cut(output, ": ")
		return "image " + strings.TrimSuffix(about, ".")
	}
	if output == "[empty file]" {
		return "empty file"
	}
	if _, about, ok := strings.Cut(output, " is not a text file: "); ok && !strings.Contains(output, "\n") {
		return about // a binary file's description is all the model got
	}
	if i := strings.LastIndex(output, "\n--- truncated: "); i >= 0 {
		note := strings.TrimSuffix(output[i+len("\n--- truncated: "):], " ---")
		if shown, _, ok := strings.Cut(strings.TrimPrefix(note, "showing "), "; call read_file"); ok && strings.HasPrefix(note, "showing ") {
			return shown // "lines 1-2000 of 5120"
		}
		output = output[:i]
	}
	lines := strings.Split(output, "\n")
	first, last := readLineNumber.FindStringSubmatch(lines[0]), readLineNumber.FindStringSubmatch(lines[len(lines)-1])
	if first != nil && last != nil && first[1] != "1" {
		return "lines " + first[1] + "-" + last[1]
	}
	return plural(len(lines), "line")
}

func summarizeList(output string) string {
	if output == "no files found" {
		return "no files"
	}
	entries, cut := strings.Split(output, "\n"), ""
	if strings.HasPrefix(entries[len(entries)-1], "[truncated at ") {
		entries, cut = entries[:len(entries)-1], ", cut short"
	}
	dirs := 0
	for _, e := range entries {
		if strings.HasSuffix(e, "/") {
			dirs++
		}
	}
	if dirs == 0 {
		return plural(len(entries), "file") + cut
	}
	return plural(len(entries)-dirs, "file") + ", " + plural(dirs, "directory") + cut
}

// grepMatchLine matches a content-mode line that matched, as path:line:text; context lines
// use dashes.
var grepMatchLine = regexp.MustCompile(`^(.+?):(\d+):`)

func summarizeGrep(argsJSON, output string) string {
	// The first line restates the search, which the call line already shows.
	if strings.HasPrefix(output, "[searched for ") {
		_, output, _ = strings.Cut(output, "\n")
	}
	if output == "no matches found" {
		return "no matches"
	}
	lines, more := strings.Split(output, "\n"), ""
	if strings.HasPrefix(lines[len(lines)-1], "[stopped after ") {
		lines, more = lines[:len(lines)-1], ", more not shown"
	}
	var args struct {
		OutputMode string `json:"output_mode"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		args.OutputMode = "" // display only: grep itself rejects bad arguments
	}
	switch args.OutputMode {
	case "count":
		total := 0
		for _, l := range lines {
			if i := strings.LastIndexByte(l, ':'); i >= 0 {
				if n, err := strconv.Atoi(l[i+1:]); err == nil {
					total += n
				}
			}
		}
		return plural(total, "match") + " in " + plural(len(lines), "file") + more
	case "content":
		files, matches := map[string]bool{}, 0
		for _, l := range lines {
			if m := grepMatchLine.FindStringSubmatch(l); m != nil {
				files[m[1]] = true
				matches++
			}
		}
		return plural(matches, "matching line") + " in " + plural(len(files), "file") + more
	}
	return plural(len(lines), "file") + more
}

func summarizeFetch(output string) string {
	parts := []string{formatBytes(len(output)), countLines(output)}
	if after, ok := strings.CutPrefix(output, "[redirected to "); ok {
		target, _, _ := strings.Cut(after, "]")
		if u, err := url.Parse(target); err == nil && u.Host != "" {
			parts = append(parts, "redirected to "+u.Host)
		}
	}
	if strings.Contains(output, "\n\n... [truncated to ") {
		parts = append(parts, "cut short")
	}
	return strings.Join(parts, ", ")
}

// searchHeader and searchResult match web_search's header line and each result's title line.
var (
	searchHeader = regexp.MustCompile(`^Search results for .* from (.+) \(today is [^,)]*(, the same search [^)]*)?\):$`)
	searchResult = regexp.MustCompile(`^\d+\. \[(.*)\]\((.*)\)$`)
)

func summarizeSearch(output string) (string, []string) {
	lines := strings.Split(output, "\n")
	h := searchHeader.FindStringSubmatch(lines[0])
	if h == nil {
		return firstLine(output), nil
	}
	var results []string
	for _, l := range lines[1:] {
		if m := searchResult.FindStringSubmatch(l); m != nil {
			title := clipText(m[1], 90)
			if u, err := url.Parse(m[2]); err == nil && u.Host != "" {
				title += theme.ASCII(" · ") + strings.TrimPrefix(u.Host, "www.")
			}
			results = append(results, title)
		}
	}
	summary := plural(len(results), "result") + " from " + h[1]
	if h[2] != "" {
		summary += " (cached)"
	}
	return summary, results
}

// commandCallLines is how many lines of a run_command call the transcript shows: a script
// written inline, such as a heredoc, can run to dozens. The turn browser shows it whole.
const commandCallLines = 3

// commandHead returns the first commandCallLines lines of a command and how many lines follow
// them. A single line more is shown rather than replaced by a note of its own.
func commandHead(command string) (string, int) {
	lines := strings.Split(strings.TrimRight(command, "\n"), "\n")
	if len(lines) <= commandCallLines+1 {
		return strings.Join(lines, "\n"), 0
	}
	return strings.Join(lines[:commandCallLines], "\n"), len(lines) - commandCallLines
}

// commandTail returns the last commandTailLines lines of a command's output and how many lines
// came before them.
func commandTail(output string) (string, int) {
	lines := strings.Split(output, "\n")
	hidden := max(0, len(lines)-commandTailLines)
	return strings.Join(lines[hidden:], "\n"), hidden
}

func countLines(s string) string { return plural(strings.Count(s, "\n")+1, "line") }

// plural writes n with the noun, adding "s" or "es", or turning a final "y" into "ies".
func plural(n int, noun string) string {
	if n != 1 {
		switch {
		case strings.HasSuffix(noun, "ch"):
			noun += "es"
		case strings.HasSuffix(noun, "y"):
			noun = strings.TrimSuffix(noun, "y") + "ies"
		default:
			noun += "s"
		}
	}
	return FormatNumber(n) + " " + noun
}

// formatBytes writes a size as 812 B, 12 KB or 1.4 MB.
func formatBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", n/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return clipText(line, 100)
}

// clipText cuts s to at most n characters, ending with an ellipsis when it was cut.
func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
