// Package redact masks credentials in text that leaves the process: saved
// sessions, provider error messages and the audit log. It recognises common
// token formats, private key blocks and bearer tokens, plus any exact secret
// registered with Register (the configured API keys).
package redact

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Placeholder replaces each redacted secret.
const Placeholder = "[REDACTED]"

// minRegisteredLen is the shortest registered secret that is masked; shorter
// values would match ordinary text.
const minRegisteredLen = 8

// rule is one credential format. Every pattern starts with a literal so the
// regexp engine can skip quickly to candidate positions; a leading word boundary
// is checked by hand (\b would disable that optimisation).
type rule struct {
	re        *regexp.Regexp
	wordStart bool // the match must not follow a letter, digit or underscore
	keep      int  // leading bytes of the match kept in the output (e.g. "Bearer")
}

var rules = []rule{
	{re: regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`), wordStart: true},        // OpenAI and Anthropic keys
	{re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), wordStart: true},        // Google API keys
	{re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), wordStart: true},   // GitHub tokens
	{re: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`), wordStart: true}, // GitHub fine-grained tokens
	{re: regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`), wordStart: true}, // Slack tokens
	{re: regexp.MustCompile(`AKIA[0-9A-Z]{16}\b`), wordStart: true},           // AWS access key IDs
	{re: regexp.MustCompile(`ASIA[0-9A-Z]{16}\b`), wordStart: true},           // AWS temporary key IDs
	{re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?s:.*?)-----END [A-Z ]*PRIVATE KEY-----`)},
	// Authorization header values, in the spellings seen in practice.
	{re: regexp.MustCompile(`Bearer\s+[A-Za-z0-9._~+/-]{16,}=*`), wordStart: true, keep: len("Bearer")},
	{re: regexp.MustCompile(`bearer\s+[A-Za-z0-9._~+/-]{16,}=*`), wordStart: true, keep: len("bearer")},
	{re: regexp.MustCompile(`BEARER\s+[A-Za-z0-9._~+/-]{16,}=*`), wordStart: true, keep: len("BEARER")},
}

var (
	mu       sync.RWMutex
	replacer *strings.Replacer // masks the registered secrets; nil when there are none
	known    []string
)

// Register adds exact secrets, such as resolved API keys, to be masked wherever
// they appear. Values shorter than 8 bytes, or containing quotes, backslashes or
// control characters (which JSON would escape), are ignored.
func Register(secrets ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, s := range secrets {
		if len(s) >= minRegisteredLen && !strings.ContainsFunc(s, needsJSONEscape) && !slices.Contains(known, s) {
			known = append(known, s)
		}
	}
	// Longest first, so a secret that contains another is masked whole.
	slices.SortFunc(known, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(known))
	for _, s := range known {
		pairs = append(pairs, s, Placeholder)
	}
	replacer = nil
	if len(pairs) > 0 {
		replacer = strings.NewReplacer(pairs...)
	}
}

func needsJSONEscape(r rune) bool {
	return r == '"' || r == '\\' || r < 0x20
}

// String returns s with every recognised credential replaced by Placeholder.
func String(s string) string {
	mu.RLock()
	r := replacer
	mu.RUnlock()
	if r != nil {
		s = r.Replace(s)
	}
	for _, r := range rules {
		s = r.apply(s)
	}
	return s
}

// apply replaces the rule's matches in s.
func (r rule) apply(s string) string {
	locs := r.re.FindAllStringIndex(s, -1)
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		if r.wordStart && loc[0] > 0 && isWordByte(s[loc[0]-1]) {
			continue
		}
		if b.Len() == 0 {
			b.Grow(len(s))
		}
		b.WriteString(s[last : loc[0]+r.keep])
		if r.keep > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(Placeholder)
		last = loc[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func isWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c|0x20 && c|0x20 <= 'z'
}

// Bytes is String for byte slices. The result contains no quote or backslash that
// was not in the input, so it is safe to apply to encoded JSON.
func Bytes(b []byte) []byte {
	return []byte(String(string(b)))
}
