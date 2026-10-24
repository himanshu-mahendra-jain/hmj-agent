package tools

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// searchRecencies are the values web_search's recency takes, from the narrowest.
var searchRecencies = []string{"day", "week", "month", "year"}

// The search engines' own names for each recency. Bing has none for a year.
var (
	ddgRecency   = map[string]string{"day": "d", "week": "w", "month": "m", "year": "y"}
	braveRecency = map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}
	bingRecency  = map[string]string{"day": `ex1:"ez1"`, "week": `ex1:"ez2"`, "month": `ex1:"ez3"`}
)

// normalizeDomains turns the domains a call names, which may be written as URLs, into
// lowercase host names without "www.", dropping empty and repeated ones.
func normalizeDomains(field string, domains []string) ([]string, error) {
	var out []string
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if strings.Contains(d, "://") {
			u, err := url.Parse(d)
			if err != nil {
				return nil, fmt.Errorf("invalid %s entry %q: %w", field, d, err)
			}
			d = u.Hostname()
		}
		d, _, _ = strings.Cut(d, "/")
		d = strings.TrimPrefix(strings.TrimPrefix(d, "*."), "www.")
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, " \t:?#") || !strings.Contains(d, ".") {
			return nil, fmt.Errorf("invalid %s entry %q: give a domain such as docs.python.org", field, d)
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// engineText returns the query as the search engines take it: the text with site: operators
// for the allowed domains and -site: for the blocked ones.
func (q searchQuery) engineText() string {
	var text strings.Builder
	text.WriteString(q.text)
	switch len(q.allowed) {
	case 0:
	case 1:
		text.WriteString(" site:" + q.allowed[0])
	default:
		text.WriteString(" (site:" + strings.Join(q.allowed, " OR site:") + ")")
	}
	for _, d := range q.blocked {
		text.WriteString(" -site:" + d)
	}
	return text.String()
}

// keeps reports whether a result's URL passes the call's domain filters; the engines do not
// always honour site: operators, so their results are checked again.
func (q searchQuery) keeps(r SearchResult) bool {
	u, err := url.Parse(r.URL)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	in := func(domains []string) bool {
		return slices.ContainsFunc(domains, func(d string) bool { return host == d || strings.HasSuffix(host, "."+d) })
	}
	return (len(q.allowed) == 0 || in(q.allowed)) && !in(q.blocked)
}
