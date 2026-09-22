package manifest_test

import (
	"path"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// overLongPath is a path longer than any filesystem accepts, used as a
// known-bad seed.
var overLongPath = strings.Repeat("a", 5000)

// FuzzValidRelPath checks the rule that decides which paths a peer may name.
// The invariant is containment: a path ValidRelPath accepts, joined to a
// folder root, resolves to something under that root, and it names neither the
// state directory nor the ignore file.
func FuzzValidRelPath(f *testing.F) {
	seeds := []string{
		// Valid shapes.
		"notes.txt",
		"a/b/c.txt",
		"documentos/café/résumé.txt",
		"..hidden",
		".config/settings",
		// Known-bad shapes.
		"",
		"..",
		"../escape.txt",
		"a/../../x",
		"/abs",
		"/../etc/passwd",
		".",
		"a//b",
		"a/",
		`C:\Windows\system32`,
		`..\..\escape.txt`,
		"a\x00b",
		"\x00",
		manifest.ReservedDir,
		manifest.ReservedDir + "/partial/x",
		manifest.IgnoreFile,
		overLongPath,
		overLongPath + "/" + overLongPath,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, p string) {
		err := manifest.ValidRelPath(p)
		if err != nil {
			// A rejection is the safe answer; it only has to be
			// stable, so two calls never disagree about a path.
			if manifest.ValidRelPath(p) == nil {
				t.Fatalf("ValidRelPath(%q) is not deterministic", p)
			}
			return
		}

		switch {
		case p == "":
			t.Fatalf("ValidRelPath accepted the empty path")
		case strings.ContainsRune(p, 0):
			t.Fatalf("ValidRelPath(%q) accepted a NUL byte", p)
		case strings.HasPrefix(p, "/"):
			t.Fatalf("ValidRelPath(%q) accepted an absolute path", p)
		}

		// Containment: joining an accepted path to a root stays under
		// the root, and the path is already in its cleaned form, so no
		// later Clean can move it.
		if got := path.Clean(p); got != p {
			t.Fatalf("ValidRelPath(%q) accepted an uncleaned path, Clean gives %q", p, got)
		}
		const root = "/folders/photos"
		joined := path.Join(root, p)
		if !strings.HasPrefix(joined, root+"/") {
			t.Fatalf("ValidRelPath(%q) escapes the root: %q", p, joined)
		}

		// The reserved names belong to this device alone.
		first, _, _ := strings.Cut(p, "/")
		if first == manifest.ReservedDir || first == manifest.IgnoreFile {
			t.Fatalf("ValidRelPath(%q) accepted a reserved path", p)
		}
	})
}

// FuzzValidLinkTarget checks that an accepted symlink target resolves to a
// path inside the folder that is not reserved.
func FuzzValidLinkTarget(f *testing.F) {
	seeds := []struct{ link, target string }{
		// Valid shapes.
		{"link", "notes.txt"},
		{"sub/link", "../notes.txt"},
		{"sub/deep/link", "../../a/b.txt"},
		// Known-bad shapes.
		{"link", ""},
		{"link", "/etc/passwd"},
		{"link", "../outside"},
		{"sub/link", "../../outside"},
		{"link", "a\x00b"},
		{"link", manifest.ReservedDir + "/x"},
		{"link", `..\..\escape`},
		{"../link", "notes.txt"},
		{"link", overLongPath},
	}
	for _, s := range seeds {
		f.Add(s.link, s.target)
	}

	f.Fuzz(func(t *testing.T, link, target string) {
		if manifest.ValidLinkTarget(link, target) != nil {
			return
		}

		// An accepted target implies an accepted link path.
		if err := manifest.ValidRelPath(link); err != nil {
			t.Fatalf("ValidLinkTarget(%q, %q) accepted an invalid link path: %v", link, target, err)
		}
		if strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
			t.Fatalf("ValidLinkTarget(%q, %q) accepted an absolute target or a NUL byte", link, target)
		}

		resolved := path.Join(path.Dir(link), target)
		if resolved == ".." || strings.HasPrefix(resolved, "../") {
			t.Fatalf("ValidLinkTarget(%q, %q) resolves to %q, outside the folder", link, target, resolved)
		}
		const root = "/folders/photos"
		joined := path.Join(root, resolved)
		if joined != root && !strings.HasPrefix(joined, root+"/") {
			t.Fatalf("ValidLinkTarget(%q, %q) escapes the root: %q", link, target, joined)
		}
		if first, _, _ := strings.Cut(resolved, "/"); resolved != "." &&
			(first == manifest.ReservedDir || first == manifest.IgnoreFile) {
			t.Fatalf("ValidLinkTarget(%q, %q) reaches the reserved path %q", link, target, resolved)
		}
	})
}
