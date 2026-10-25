package tools

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"

	"hmj-agent/v2/internal/chat"
)

var errLimitReached = errors.New("limit_reached")

// listFilesMaxResults caps the number of entries returned by list_files.
const listFilesMaxResults = 300

type ListFilesTool struct{}

func (t *ListFilesTool) Name() string { return "list_files" }

func (t *ListFilesTool) Definition() chat.ToolDefinition {
	return chat.ToolDefinition{
		Type: "function",
		Function: chat.FunctionDefinition{
			Name:        "list_files",
			Description: "List files and directories within a given path. Skips .git directory and respects .gitignore rules. With pattern, lists only the files it matches, at any depth.",
			Parameters: map[string]any{
				"type":             "object",
				chat.PropertyOrder: []string{"path", "recursive", "max_depth", "pattern"},
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Directory path to list: relative to the current working directory, or absolute (default '.'). Any path on the machine is allowed.",
					},
					"recursive": map[string]any{
						"type":        "boolean",
						"description": "Whether to list subdirectories recursively (default true).",
					},
					"max_depth": map[string]any{
						"type":        "integer",
						"description": "Maximum directory depth when recursive is true (default 4, or no limit with pattern).",
					},
					"pattern": map[string]any{
						"type":        "string",
						"description": "Optional glob the files listed must match: a name pattern such as `*.go` or `*.{ts,tsx}`, or, with a '/', a path pattern relative to path such as `src/**/*Test.kt`.",
					},
				},
				"required":             []string{},
				"additionalProperties": false,
			},
		},
	}
}

type listFilesInput struct {
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
	MaxDepth  int    `json:"max_depth"`
	Pattern   string `json:"pattern"`
}

type gitIgnoreRule struct {
	baseDir               string
	negated               bool
	onlyDir               bool
	anchored              bool
	patternSegments       []string
	trailingSlashStarStar bool
	singlePattern         string
}

func convertNegatedCharClass(s string) string {
	if !strings.Contains(s, "[!") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '[' && i+1 < len(s) && s[i+1] == '!' {
			slashes := 0
			for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
				slashes++
			}
			if slashes%2 == 0 {
				b.WriteString("[^")
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// trimGitIgnoreLine strips a trailing CR and any unescaped trailing spaces.
func trimGitIgnoreLine(rawLine string) string {
	line := strings.TrimRight(rawLine, "\r")

	// Trailing spaces are ignored unless escaped with backslash
	for len(line) > 0 && line[len(line)-1] == ' ' {
		slashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			slashes++
		}
		if slashes%2 == 1 {
			line = line[:len(line)-2] + " "
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

// stripGitIgnoreNegation removes a leading "!" (or escaped "\!") and reports whether
// the pattern was a negation.
func stripGitIgnoreNegation(line string) (string, bool) {
	if strings.HasPrefix(line, "!") {
		return line[1:], true
	}
	if strings.HasPrefix(line, `\!`) {
		return line[1:], false
	}
	return line, false
}

// splitGitIgnoreSegments splits a pattern on "/", dropping empty segments and
// collapsing repeated "**" segments.
func splitGitIgnoreSegments(line string) []string {
	var segs []string
	for s := range strings.SplitSeq(line, "/") {
		if s == "" {
			continue
		}
		s = convertNegatedCharClass(s)
		if s == "**" && len(segs) > 0 && segs[len(segs)-1] == "**" {
			continue
		}
		segs = append(segs, s)
	}
	return segs
}

// finalizeGitIgnoreSegments prepends "**" to unanchored patterns and returns the
// single-segment pattern used for fast matching, if the pattern has just one segment.
func finalizeGitIgnoreSegments(segs []string, anchored bool) ([]string, string) {
	var singlePattern string
	if len(segs) == 1 && segs[0] != "**" {
		singlePattern = segs[0]
	}
	if !anchored && segs[0] != "**" {
		segs = append([]string{"**"}, segs...)
	}
	return segs, singlePattern
}

func parseGitIgnoreLine(baseDir, rawLine string) (*gitIgnoreRule, bool) {
	line := trimGitIgnoreLine(rawLine)

	if len(line) == 0 || line[0] == '#' {
		return nil, false
	}
	if strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}

	line, negated := stripGitIgnoreNegation(line)
	if len(line) == 0 {
		return nil, false
	}

	onlyDir := false
	if strings.HasSuffix(line, "/") {
		onlyDir = true
		line = strings.TrimSuffix(line, "/")
	}

	if len(line) == 0 {
		return nil, false
	}

	trailingSlashStarStar := false
	if strings.HasSuffix(line, "/**") && len(line) > 3 {
		trailingSlashStarStar = true
	}

	anchored := false
	if strings.HasPrefix(line, "/") {
		anchored = true
		line = strings.TrimPrefix(line, "/")
	} else if strings.Contains(line, "/") {
		if !strings.HasPrefix(line, "**/") {
			anchored = true
		}
	}

	segs := splitGitIgnoreSegments(line)
	if len(segs) == 0 {
		return nil, false
	}

	segs, singlePattern := finalizeGitIgnoreSegments(segs, anchored)

	return &gitIgnoreRule{
		baseDir:               baseDir,
		negated:               negated,
		onlyDir:               onlyDir,
		anchored:              anchored,
		patternSegments:       segs,
		trailingSlashStarStar: trailingSlashStarStar,
		singlePattern:         singlePattern,
	}, true
}

func matchSegments(pSegs, tSegs []string, trailingStarStar bool) bool {
	for len(pSegs) > 0 {
		if pSegs[0] == "**" {
			if len(pSegs) == 1 {
				if trailingStarStar {
					return len(tSegs) > 0
				}
				return true
			}
			for i := 0; i <= len(tSegs); i++ {
				if matchSegments(pSegs[1:], tSegs[i:], trailingStarStar) {
					return true
				}
			}
			return false
		}

		if len(tSegs) == 0 {
			return false
		}

		matched, err := path.Match(pSegs[0], tSegs[0])
		if err != nil || !matched {
			return false
		}

		pSegs = pSegs[1:]
		tSegs = tSegs[1:]
	}
	return len(tSegs) == 0
}

func (r *gitIgnoreRule) applies(targetPath string, isDir bool) bool {
	if r.onlyDir && !isDir {
		return false
	}

	var relPath string
	if r.baseDir == "" {
		relPath = targetPath
	} else {
		if targetPath == r.baseDir {
			relPath = ""
		} else if after, ok := strings.CutPrefix(targetPath, r.baseDir+"/"); ok {
			relPath = after
		} else {
			return false
		}
	}

	if relPath == "" {
		return false
	}

	if r.singlePattern != "" {
		if !r.anchored {
			base := relPath
			if idx := strings.LastIndexByte(relPath, '/'); idx >= 0 {
				base = relPath[idx+1:]
			}
			matched, err := path.Match(r.singlePattern, base)
			return err == nil && matched
		}

		if strings.IndexByte(relPath, '/') != -1 {
			return false
		}
		matched, err := path.Match(r.singlePattern, relPath)
		return err == nil && matched
	}

	tSegs := strings.Split(relPath, "/")
	return matchSegments(r.patternSegments, tSegs, r.trailingSlashStarStar)
}

type gitIgnoreMatcher struct {
	rules []*gitIgnoreRule
}

func newGitIgnoreMatcher() *gitIgnoreMatcher {
	return &gitIgnoreMatcher{
		rules: make([]*gitIgnoreRule, 0, 32),
	}
}

func (m *gitIgnoreMatcher) addRules(baseDir, content string) {
	lines := strings.SplitSeq(content, "\n")
	for line := range lines {
		if rule, ok := parseGitIgnoreLine(baseDir, line); ok {
			m.rules = append(m.rules, rule)
		}
	}
}

func (m *gitIgnoreMatcher) Match(targetPath string, isDir bool) bool {
	ignored := false
	for _, rule := range m.rules {
		if rule.applies(targetPath, isDir) {
			ignored = !rule.negated
		}
	}
	return ignored
}

// parseListFilesArgs decodes the tool arguments; empty or "{}" input yields zero values.
func parseListFilesArgs(argsJSON string) (listFilesInput, error) {
	var in listFilesInput
	if argsJSON != "" && argsJSON != "{}" {
		if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
			return in, fmt.Errorf("invalid JSON arguments for list_files: %w", err)
		}
	}
	return in, nil
}

// listFilesBase returns the directory that gitignore rules are anchored at and the
// walk root relative to it. Rules are anchored at the working directory when the
// target is inside it; otherwise the target directory itself becomes the anchor.
func listFilesBase(absRoot string) (string, string, error) {
	base, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	relRoot, err := filepath.Rel(base, absRoot)
	if err != nil || !filepath.IsLocal(relRoot) {
		return absRoot, ".", nil
	}
	return base, relRoot, nil
}

// loadListFilesMatcher collects the .gitignore rules from the anchor down to relRoot.
// The boolean result is true when an ancestor directory of relRoot is itself ignored.
func loadListFilesMatcher(fsys fs.FS, relRoot string) (*gitIgnoreMatcher, bool) {
	matcher := newGitIgnoreMatcher()
	// Load root .gitignore
	if data, err := fs.ReadFile(fsys, ".gitignore"); err == nil {
		matcher.addRules("", string(data))
	}
	// Load any .gitignore along the path from the anchor to relRoot
	if relRoot != "." {
		parts := strings.SplitSeq(filepath.ToSlash(relRoot), "/")
		accum := ""
		for part := range parts {
			if part == "" || part == "." {
				continue
			}
			accum = path.Join(accum, part)
			if matcher.Match(accum, true) {
				return matcher, true
			}
			if data, err := fs.ReadFile(fsys, path.Join(accum, ".gitignore")); err == nil {
				matcher.addRules(accum, string(data))
			}
		}
	}
	return matcher, false
}

// listFilesWalker holds the state of a single list_files directory walk.
type listFilesWalker struct {
	fsys      fs.FS
	walkPath  string
	matcher   *gitIgnoreMatcher
	recursive bool
	maxDepth  int
	// glob, when set, is the test files must pass, given their path relative to walkPath;
	// directories are then not listed.
	glob    func(string) bool
	results []string
}

// visit is the fs.WalkDirFunc for list_files. It skips .git and ignored paths and
// stops the walk with errLimitReached once the result cap is hit.
func (w *listFilesWalker) visit(walkedPath string, d fs.DirEntry, err error) error {
	if err != nil {
		if walkedPath == w.walkPath {
			return err
		}
		if d != nil && d.IsDir() {
			return filepath.SkipDir
		}
		return err
	}

	if walkedPath == w.walkPath {
		return nil
	}

	normWalkedPath := filepath.ToSlash(walkedPath)
	if d.Name() == ".git" {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}

	rel, relErr := filepath.Rel(w.walkPath, walkedPath)
	if relErr != nil {
		return relErr
	}
	if rel == "." {
		return nil
	}
	rel = filepath.ToSlash(rel)

	if d.IsDir() {
		if err := w.visitDir(normWalkedPath, rel); err != nil {
			return err
		}
	} else if !w.visitFile(normWalkedPath, rel) {
		return nil
	}

	if len(w.results) >= listFilesMaxResults {
		return errLimitReached
	}
	return nil
}

// visitDir records a directory entry, returning filepath.SkipDir when the walk should
// not descend into it. It also loads any .gitignore found inside the directory.
func (w *listFilesWalker) visitDir(normPath, rel string) error {
	if w.matcher.Match(normPath, true) {
		return filepath.SkipDir
	}
	if data, err := fs.ReadFile(w.fsys, path.Join(normPath, ".gitignore")); err == nil {
		w.matcher.addRules(normPath, string(data))
	}
	depth := strings.Count(rel, "/") + 1
	if !w.recursive && depth > 1 {
		return filepath.SkipDir
	}
	if w.recursive && depth >= w.maxDepth {
		return filepath.SkipDir
	}
	if w.glob == nil {
		w.results = append(w.results, rel+"/")
	}
	return nil
}

// visitFile records a file entry and reports whether it was included in the results.
func (w *listFilesWalker) visitFile(normPath, rel string) bool {
	if w.matcher.Match(normPath, false) {
		return false
	}
	depth := strings.Count(rel, "/") + 1
	if !w.recursive && depth > 1 {
		return false
	}
	if w.glob != nil && !w.glob(rel) {
		return false
	}
	w.results = append(w.results, rel)
	return true
}

// walkListFiles walks relRoot within fsys and returns the collected entries and
// whether the result cap truncated the listing.
func walkListFiles(fsys fs.FS, relRoot string, matcher *gitIgnoreMatcher, recursive bool, maxDepth int, glob func(string) bool) ([]string, bool, error) {
	w := &listFilesWalker{
		fsys:      fsys,
		walkPath:  relRoot,
		matcher:   matcher,
		recursive: recursive,
		maxDepth:  maxDepth,
		glob:      glob,
		results:   make([]string, 0, 64),
	}
	err := fs.WalkDir(fsys, relRoot, w.visit)

	truncated := false
	if errors.Is(err, errLimitReached) {
		truncated = true
	} else if err != nil {
		return nil, false, fmt.Errorf("failed to list files: %w", err)
	}
	return w.results, truncated, nil
}

// formatListFilesResult joins the entries into the tool output, noting truncation.
func formatListFilesResult(results []string, truncated bool) string {
	if len(results) == 0 {
		return "no files found"
	}

	out := strings.Join(results, "\n")
	if truncated {
		out += fmt.Sprintf("\n[truncated at %d entries]", listFilesMaxResults)
	}
	return out
}

func (t *ListFilesTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	in, err := parseListFilesArgs(argsJSON)
	if err != nil {
		return "", err
	}
	var glob func(string) bool
	if in.Pattern != "" {
		if glob, err = compilePathGlob(in.Pattern); err != nil {
			return "", err
		}
	}

	absRoot, err := ResolvePath(in.Path)
	if err != nil {
		return "", err
	}

	base, relRoot, err := listFilesBase(absRoot)
	if err != nil {
		return "", err
	}
	fsys := os.DirFS(base)

	normRelRoot := filepath.ToSlash(relRoot)
	if normRelRoot == ".git" || strings.HasPrefix(normRelRoot, ".git/") {
		return "no files found", nil
	}

	if _, err := fs.Stat(fsys, relRoot); err != nil {
		return "", fmt.Errorf("path not found: %s: %w", in.Path, err)
	}

	matcher, ignored := loadListFilesMatcher(fsys, relRoot)
	if ignored {
		return "no files found", nil
	}

	recursive := true
	if in.Recursive != nil {
		recursive = *in.Recursive
	}

	maxDepth := cmp.Or(in.MaxDepth, 4)
	if glob != nil && in.MaxDepth == 0 {
		maxDepth = math.MaxInt // a pattern such as **/*Test.kt looks at every depth
	}

	results, truncated, err := walkListFiles(fsys, relRoot, matcher, recursive, maxDepth, glob)
	if err != nil {
		return "", err
	}
	return formatListFilesResult(results, truncated), nil
}
