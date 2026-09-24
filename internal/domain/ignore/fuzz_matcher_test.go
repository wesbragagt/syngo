package ignore_test

import (
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/ignore"
)

// FuzzMatcher compiles arbitrary ignore files and matches arbitrary paths
// against them. The invariants are that matching is total (every path gets an
// answer, no panic), deterministic, insensitive to a leading slash or a
// trailing slash on the path, and that an empty rule set ignores nothing.
func FuzzMatcher(f *testing.F) {
	seeds := []struct{ patterns, path string }{
		// Valid shapes.
		{"*.log\nbuild/\n!keep.log", "app.log"},
		{"# comment\n\n/root-only", "root-only"},
		{"**/tmp/**", "a/tmp/b/c"},
		{"docs/*.md", "docs/readme.md"},
		{"a?c", "abc"},
		// Known-bad or degenerate shapes.
		{"!", "x"},
		{"/", "x"},
		{"!/", "x"},
		{"[", "x"},
		{"*[", "x"},
		{"\\", "x"},
		{"(|)*+?", "x"},
		{"**/**/**", strings.Repeat("a/", 40) + "b"},
		{"a", ""},
		{"a", "/"},
		{"a", "a\x00b"},
		{"a", "../escape"},
		{"a", strings.Repeat("a", 5000)},
	}
	for _, s := range seeds {
		f.Add(s.patterns, s.path)
	}

	f.Fuzz(func(t *testing.T, patterns, relPath string) {
		lines := strings.Split(patterns, "\n")
		m, err := ignore.Parse(lines)
		if err != nil {
			// A pattern that does not compile must yield no matcher.
			if m != nil {
				t.Fatalf("Parse(%q) returned both a matcher and the error %v", patterns, err)
			}
			return
		}

		for _, isDir := range []bool{false, true} {
			got := m.Match(relPath, isDir)
			if again := m.Match(relPath, isDir); again != got {
				t.Fatalf("Match(%q, %v) is not deterministic", relPath, isDir)
			}

			// A leading slash is stripped before matching, so it
			// cannot change the verdict.
			if slashed := m.Match("/"+relPath, isDir); slashed != got {
				t.Fatalf("Match(%q, %v) = %v but Match(%q, %v) = %v",
					relPath, isDir, got, "/"+relPath, isDir, slashed)
			}

			// An empty rule set ignores nothing, whatever the path.
			if empty := ignore.Combine().Match(relPath, isDir); empty {
				t.Fatalf("an empty matcher ignored %q", relPath)
			}

			// Combining a matcher with nothing is the same matcher.
			if combined := ignore.Combine(m, nil).Match(relPath, isDir); combined != got {
				t.Fatalf("Combine(m, nil).Match(%q, %v) = %v, want %v", relPath, isDir, combined, got)
			}
		}
	})
}
