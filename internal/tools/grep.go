package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"hmj-agent/v2/internal/chat"
)

const (
	// grepDefaultLimit is how many files, count lines or content lines grep returns when the
	// call sets no head_limit; grepMaxLimit is the most a head_limit may ask for.
	grepDefaultLimit = 250
	grepMaxLimit     = 2000
	// grepMaxContext caps the context lines shown before and after each match.
	grepMaxContext = 20
	// grepMaxLineBytes caps one shown line; minified files would otherwise fill the output.
	grepMaxLineBytes = 500
	// grepBinarySniffBytes is how much of a file's start is checked for a NUL byte, which
	// marks it as binary and skips it.
	grepBinarySniffBytes = 8 * 1024
)

// errGrepLimit stops the walk once the output is full.
var errGrepLimit = errors.New("grep limit reached")

type GrepTool struct{}

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name: "grep",
			Description: "Search file contents with a regular expression, under a directory (recursively, skipping .git, binary files and what .gitignore ignores) or in one file. " +
				"Returns the matching files by default; output_mode content returns the matching lines as path:line:text, with optional context lines, and count returns path:matches. " +
				"Use it rather than grep or rg through run_command.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"pattern", "path", "glob", "output_mode", "context", "case_insensitive", "literal", "head_limit"},
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Go (RE2) regular expression to find in each line, e.g. `func \\w+Handler` or `TODO|FIXME`. Escape ( [ { . * + ? | \\ for a literal, or set literal.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Directory to search recursively, or a file: relative to the current working directory, or absolute (default '.').",
					},
					"glob": map[string]any{
						"type":        "string",
						"description": "Optional filter on the files searched: a name pattern such as `*.go` or `*.{ts,tsx}`, or, with a '/', a path pattern relative to path such as `src/**/*Test.kt`.",
					},
					"output_mode": map[string]any{
						"type":        "string",
						"enum":        []string{"files_with_matches", "content", "count"},
						"description": "files_with_matches (default): the paths of matching files. content: the matching lines. count: the number of matching lines per file.",
					},
					"context": map[string]any{
						"type":        "integer",
						"description": "With output_mode content: how many lines to show before and after each match (default 0, at most 20).",
					},
					"case_insensitive": map[string]any{
						"type":        "boolean",
						"description": "Match regardless of letter case (default false).",
					},
					"literal": map[string]any{
						"type":        "boolean",
						"description": "Treat pattern as plain text rather than a regular expression (default false).",
					},
					"head_limit": map[string]any{
						"type":        "integer",
						"description": "How many files, count lines or content lines to return at most (default 250, at most 2000). The output is also capped at 64KB.",
					},
				},
				"required":             []string{"pattern"},
				"additionalProperties": false,
			},
		},
	}
}

type grepInput struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	OutputMode      string `json:"output_mode"`
	Context         int    `json:"context"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Literal         bool   `json:"literal"`
	HeadLimit       int    `json:"head_limit"`
}

// grepSearch holds one grep call's settings and the output collected so far.
type grepSearch struct {
	ctx     context.Context
	re      *regexp.Regexp
	mode    string
	context int
	limit   int

	out       strings.Builder
	entries   int // files, count lines or content lines written
	truncated bool
	tooLarge  int // files skipped for being over MaxReadableFileSize
}

func (t *GrepTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var in grepInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	expr := in.Pattern
	if in.Literal {
		expr = regexp.QuoteMeta(expr)
	}
	if in.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w; it is a Go (RE2) regular expression: escape ( [ { . * + ? | \\ for a literal, or set literal", err)
	}
	mode := cmp.Or(in.OutputMode, "files_with_matches")
	if mode != "files_with_matches" && mode != "content" && mode != "count" {
		return "", fmt.Errorf("invalid output_mode %q: use files_with_matches, content or count", in.OutputMode)
	}
	var glob func(string) bool
	if in.Glob != "" {
		if glob, err = compilePathGlob(in.Glob); err != nil {
			return "", err
		}
	}
	s := &grepSearch{
		ctx:     ctx,
		re:      re,
		mode:    mode,
		context: min(max(in.Context, 0), grepMaxContext),
		limit:   min(cmp.Or(max(in.HeadLimit, 0), grepDefaultLimit), grepMaxLimit),
	}

	absRoot, err := ResolvePath(in.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return "", fmt.Errorf("path not found: %s: %w", cmp.Or(in.Path, "."), err)
	}
	if !info.IsDir() {
		err = s.searchFile(absRoot, cmp.Or(in.Path, absRoot))
	} else {
		err = s.walk(absRoot, glob)
	}
	if err != nil && !errors.Is(err, errGrepLimit) {
		return "", err
	}
	return grepHeader(in) + "\n" + s.result(), nil
}

// grepHeader states the search as the call's arguments asked for it. A model sometimes sends
// other arguments than it meant (a session sent pattern "x" with no path four times), and,
// seeing only the results, blames something else.
func grepHeader(in grepInput) string {
	where := in.Path
	if where == "" {
		where = ". (no path given)"
	}
	h := fmt.Sprintf("[searched for %q in %s", in.Pattern, where)
	if in.Glob != "" {
		h += fmt.Sprintf(", glob %q", in.Glob)
	}
	if in.CaseInsensitive {
		h += ", case-insensitive"
	}
	if in.Literal {
		h += ", literal"
	}
	return h + "]"
}

// walk searches every file under absRoot that .gitignore keeps and glob, when set, accepts.
// Paths are shown relative to the working directory when absRoot is inside it, else absolute,
// so they can be passed to read_file as they are.
func (s *grepSearch) walk(absRoot string, glob func(string) bool) error {
	base, relRoot, err := listFilesBase(absRoot)
	if err != nil {
		return err
	}
	fsys := os.DirFS(base)
	normRoot := filepath.ToSlash(relRoot)
	if normRoot == ".git" || strings.HasPrefix(normRoot, ".git/") {
		return nil
	}
	matcher, ignored := loadListFilesMatcher(fsys, relRoot)
	if ignored {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	outside := base != wd // listFilesBase anchors at the root itself outside the working directory
	return fs.WalkDir(fsys, normRoot, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if p == normRoot {
				return err
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil // an unreadable file is passed over
		}
		if p == normRoot {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || matcher.Match(p, true) {
				return filepath.SkipDir
			}
			if data, err := fs.ReadFile(fsys, path.Join(p, ".gitignore")); err == nil {
				matcher.addRules(p, string(data))
			}
			return nil
		}
		if d.Name() == ".git" || !d.Type().IsRegular() || matcher.Match(p, false) {
			return nil
		}
		if glob != nil {
			rel := p
			if normRoot != "." {
				rel = strings.TrimPrefix(p, normRoot+"/")
			}
			if !glob(rel) {
				return nil
			}
		}
		shown := p
		if outside {
			shown = filepath.Join(absRoot, p)
		}
		return s.searchFile(filepath.Join(base, p), shown)
	})
}

// searchFile searches one file and adds what it finds to the output, showing it as shown. It
// passes over binary files, files over MaxReadableFileSize and files it cannot read, and
// returns errGrepLimit once the output is full.
func (s *grepSearch) searchFile(absPath, shown string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return nil
	}
	if info.Size() > MaxReadableFileSize {
		s.tooLarge++
		return nil
	}
	data, err := os.ReadFile(absPath)
	if err != nil || bytes.IndexByte(data[:min(len(data), grepBinarySniffBytes)], 0) >= 0 {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var matches []int
	for i, l := range lines {
		if s.re.MatchString(strings.TrimSuffix(l, "\r")) {
			matches = append(matches, i)
			if s.mode == "files_with_matches" {
				break
			}
		}
	}
	if len(matches) == 0 {
		return nil
	}
	switch s.mode {
	case "files_with_matches":
		return s.add(shown)
	case "count":
		return s.add(fmt.Sprintf("%s:%d", shown, len(matches)))
	}
	return s.addContent(shown, lines, matches)
}

// addContent writes a file's matching lines as path:line:text and, when context is asked for,
// its context lines as path-line-text, with "--" between groups that do not touch.
func (s *grepSearch) addContent(shown string, lines []string, matches []int) error {
	isMatch := make(map[int]bool, len(matches))
	for _, m := range matches {
		isMatch[m] = true
	}
	next := 0 // the first line not yet written
	for _, m := range matches {
		from, to := max(m-s.context, next), min(m+s.context, len(lines)-1)
		if from > to {
			continue
		}
		if s.context > 0 && s.entries > 0 && (next == 0 || from > next) {
			s.out.WriteString("--\n") // separates groups, not entries
		}
		for i := from; i <= to; i++ {
			sep := "-"
			if isMatch[i] {
				sep = ":"
			}
			if err := s.add(fmt.Sprintf("%s%s%d%s%s", shown, sep, i+1, sep, clipGrepLine(strings.TrimSuffix(lines[i], "\r")))); err != nil {
				return err
			}
		}
		next = to + 1
	}
	return nil
}

// add writes one entry, or returns errGrepLimit when the output is full.
func (s *grepSearch) add(entry string) error {
	if s.entries >= s.limit || s.out.Len()+len(entry)+1 > MaxCommandOutputBytes {
		s.truncated = true
		return errGrepLimit
	}
	s.out.WriteString(entry)
	s.out.WriteByte('\n')
	s.entries++
	return nil
}

// result returns the output with notes of anything left out.
func (s *grepSearch) result() string {
	out := strings.TrimSuffix(strings.TrimSuffix(s.out.String(), "--\n"), "\n")
	if out == "" {
		out = "no matches found"
	}
	if s.truncated {
		out += fmt.Sprintf("\n[stopped after %d %s; there are more matches: narrow the pattern, path or glob, or raise head_limit]", s.entries, map[string]string{
			"files_with_matches": "files", "count": "files", "content": "lines",
		}[s.mode])
	}
	if s.tooLarge > 0 {
		out += fmt.Sprintf("\n[%d file(s) over 10MB not searched]", s.tooLarge)
	}
	return out
}

// clipGrepLine cuts a line longer than grepMaxLineBytes, on a character boundary.
func clipGrepLine(l string) string {
	if len(l) <= grepMaxLineBytes {
		return l
	}
	return strings.ToValidUTF8(l[:grepMaxLineBytes], "") + fmt.Sprintf(" … [%d more bytes]", len(l)-grepMaxLineBytes)
}

// compilePathGlob returns a test for paths relative to a search root. A pattern without '/'
// matches a file's name at any depth (`*.go`); one with '/' matches the whole relative path,
// where `**` stands for any number of directories (`src/**/*Test.kt`). Braces give
// alternatives (`*.{ts,tsx}`).
func compilePathGlob(pattern string) (func(string) bool, error) {
	var tests []func(string) bool
	for _, p := range expandBraces(pattern) {
		segs := splitGitIgnoreSegments(strings.TrimPrefix(p, "./"))
		if len(segs) == 0 {
			return nil, fmt.Errorf("invalid glob %q", pattern)
		}
		for _, seg := range segs {
			if _, err := path.Match(seg, ""); err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
			}
		}
		if len(segs) == 1 && !strings.Contains(p, "/") {
			name := segs[0]
			tests = append(tests, func(rel string) bool {
				ok, err := path.Match(name, path.Base(rel)) // the pattern was checked above
				return ok && err == nil
			})
			continue
		}
		tests = append(tests, func(rel string) bool {
			return matchSegments(segs, strings.Split(rel, "/"), false)
		})
	}
	return func(rel string) bool {
		for _, test := range tests {
			if test(rel) {
				return true
			}
		}
		return false
	}, nil
}

// expandBraces turns `a{b,c}d` into `abd` and `acd`, recursively, for every brace group.
func expandBraces(p string) []string {
	open := strings.IndexByte(p, '{')
	if open < 0 {
		return []string{p}
	}
	end := strings.IndexByte(p[open:], '}')
	if end < 0 {
		return []string{p}
	}
	end += open
	var out []string
	for alt := range strings.SplitSeq(p[open+1:end], ",") {
		out = append(out, expandBraces(p[:open]+alt+p[end+1:])...)
	}
	return out
}
