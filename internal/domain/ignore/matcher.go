// Package ignore implements a subset of gitignore pattern matching: comments,
// negation, directory-only patterns, root anchoring, and the * / ** wildcards.
package ignore

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// rule is one compiled pattern line.
type rule struct {
	pattern string
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// Matcher decides whether a relative path is ignored. The last rule that
// matches a path wins, so a later negation can re-include it.
type Matcher struct {
	rules []rule
}

// Parse compiles pattern lines into a Matcher. Blank lines and lines starting
// with '#' are skipped.
func Parse(lines []string) (*Matcher, error) {
	m := &Matcher{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		r, err := compile(trimmed)
		if err != nil {
			return nil, fmt.Errorf("ignore pattern %d (%q): %w", i+1, trimmed, err)
		}
		m.rules = append(m.rules, r)
	}
	return m, nil
}

// Combine returns a Matcher applying the rules of ms in order, so patterns from
// later matchers override earlier ones.
func Combine(ms ...*Matcher) *Matcher {
	out := &Matcher{}
	for _, m := range ms {
		if m != nil {
			out.rules = append(out.rules, m.rules...)
		}
	}
	return out
}

// Match reports whether relPath is ignored. A path inside an ignored directory
// is ignored unless one of its own rules re-includes it.
func (m *Matcher) Match(relPath string, isDir bool) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}
	clean := strings.Trim(path.Clean(strings.TrimPrefix(relPath, "/")), "/")
	if clean == "" || clean == "." {
		return false
	}
	if matched, ignored := m.matchPath(clean, isDir); matched {
		return ignored
	}
	if parent := path.Dir(clean); parent != "." && parent != "/" {
		return m.Match(parent, true)
	}
	return false
}

// matchPath applies every rule to relPath and reports the verdict of the last
// rule that matched.
func (m *Matcher) matchPath(relPath string, isDir bool) (matched, ignored bool) {
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(relPath) {
			matched, ignored = true, !r.negate
		}
	}
	return matched, ignored
}

// compile turns a single gitignore-style pattern into a rule.
func compile(pattern string) (rule, error) {
	r := rule{pattern: pattern}
	body := pattern
	if strings.HasPrefix(body, "!") {
		r.negate = true
		body = body[1:]
	}
	if strings.HasSuffix(body, "/") {
		r.dirOnly = true
		body = strings.TrimSuffix(body, "/")
	}
	if body == "" {
		return rule{}, fmt.Errorf("empty pattern")
	}

	anchored := strings.HasPrefix(body, "/")
	body = strings.TrimPrefix(body, "/")
	if strings.Contains(body, "/") {
		anchored = true
	}

	expr := translate(body)
	if !anchored {
		expr = "(?:[^/]+/)*" + expr
	}
	re, err := regexp.Compile("^" + expr + "$")
	if err != nil {
		return rule{}, err
	}
	r.re = re
	return r, nil
}

// doubleStar marks a "**" segment while segments are joined, so it can be
// expanded together with its surrounding separator.
const doubleStar = "\x00"

// translate converts a glob pattern into a regular expression body.
func translate(pattern string) string {
	segments := strings.Split(pattern, "/")
	parts := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg == "**" {
			parts = append(parts, doubleStar)
			continue
		}
		parts = append(parts, translateSegment(seg))
	}
	expr := strings.Join(parts, "/")
	expr = strings.ReplaceAll(expr, doubleStar+"/", "(?:[^/]+/)*")
	expr = strings.ReplaceAll(expr, "/"+doubleStar, "(?:/[^/]+)*")
	expr = strings.ReplaceAll(expr, doubleStar, ".*")
	return expr
}

// translateSegment converts one path segment, where '*' and '?' never cross a
// separator.
func translateSegment(seg string) string {
	var b strings.Builder
	for _, c := range seg {
		switch c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
