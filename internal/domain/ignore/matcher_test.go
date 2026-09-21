package ignore_test

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/ignore"
)

func TestMatcher(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		isDir    bool
		want     bool
	}{
		{"star suffix matches at root", []string{"*.tmp"}, "a.tmp", false, true},
		{"star suffix matches at depth", []string{"*.tmp"}, "src/deep/a.tmp", false, true},
		{"star suffix misses other ext", []string{"*.tmp"}, "a.txt", false, false},
		{"ignored parent covers children", []string{"*.tmp"}, "a.tmp/b.txt", false, true},

		{"anchored dir matches at root", []string{"/build"}, "build", true, true},
		{"anchored dir misses at depth", []string{"/build"}, "src/build", true, false},
		{"anchored dir covers children", []string{"/build"}, "build/out.o", false, true},

		{"dir only matches dir", []string{"build/"}, "build", true, true},
		{"dir only misses file", []string{"build/"}, "build", false, false},
		{"dir only matches at depth", []string{"build/"}, "src/build", true, true},
		{"dir only covers children", []string{"build/"}, "build/out.o", false, true},

		{"double star prefix at root", []string{"**/node_modules"}, "node_modules", true, true},
		{"double star prefix at depth", []string{"**/node_modules"}, "a/b/node_modules", true, true},
		{"double star prefix covers children", []string{"**/node_modules"}, "a/node_modules/x/y.js", false, true},

		{"negation re-includes", []string{"*.tmp", "!keep.tmp"}, "keep.tmp", false, false},
		{"negation leaves others ignored", []string{"*.tmp", "!keep.tmp"}, "drop.tmp", false, true},

		{"anchored double star middle", []string{"docs/**/*.md"}, "docs/a.md", false, true},
		{"anchored double star deep", []string{"docs/**/x/a.md", "docs/**/*.md"}, "docs/x/y/a.md", false, true},
		{"anchored double star wrong root", []string{"docs/**/*.md"}, "src/a.md", false, false},

		{"hidden dir", []string{".git"}, ".git", true, true},
		{"hidden dir contents", []string{".git"}, ".git/config", false, true},
		{"hidden dir at depth", []string{".git"}, "vendor/.git", true, true},

		{"last match wins ignore", []string{"!a.txt", "a.txt"}, "a.txt", false, true},
		{"last match wins include", []string{"a.txt", "!a.txt"}, "a.txt", false, false},

		{"comments and blanks skipped", []string{"# comment", "", "  ", "*.log"}, "x.log", false, true},
		{"no rules", nil, "anything", false, false},
		{"root path never ignored", []string{"**"}, "", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ignore.Parse(tt.patterns)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tt.patterns, err)
			}
			if got := m.Match(tt.path, tt.isDir); got != tt.want {
				t.Fatalf("Match(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestCombine_LaterMatchersOverride(t *testing.T) {
	base, err := ignore.Parse([]string{"*.tmp"})
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	extra, err := ignore.Parse([]string{"!keep.tmp"})
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	combined := ignore.Combine(base, extra, nil)

	if !combined.Match("drop.tmp", false) {
		t.Error("drop.tmp should be ignored")
	}
	if combined.Match("keep.tmp", false) {
		t.Error("keep.tmp should be re-included")
	}
}

func TestParse_RejectsEmptyPattern(t *testing.T) {
	if _, err := ignore.Parse([]string{"!"}); err == nil {
		t.Fatal("Parse(\"!\") = nil error, want error")
	}
}
