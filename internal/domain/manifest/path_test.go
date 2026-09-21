package manifest_test

import (
	"errors"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

func TestValidRelPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{name: "plain file", path: "notes.txt", ok: true},
		{name: "nested file", path: "a/b/c.txt", ok: true},
		{name: "unicode name", path: "documentos/café/résumé.txt", ok: true},
		{name: "emoji name", path: "photos/🌞.png", ok: true},
		{name: "dot prefixed name", path: ".config/settings", ok: true},
		{name: "name starting with two dots", path: "..hidden", ok: true},
		{name: "empty", path: ""},
		{name: "parent", path: ".."},
		{name: "parent with child", path: "../escape.txt"},
		{name: "two parents", path: "../../x"},
		{name: "parent in the middle", path: "a/../../x"},
		{name: "parent segment", path: "a/../b"},
		{name: "absolute", path: "/abs"},
		{name: "absolute parent", path: "/../etc/passwd"},
		{name: "current directory", path: "."},
		{name: "dot slash", path: "./a"},
		{name: "trailing slash", path: "a/"},
		{name: "empty segment", path: "a//b"},
		{name: "nul byte", path: "a\x00b"},
		{name: "reserved directory", path: ".syncthis"},
		{name: "inside the reserved directory", path: ".syncthis/tmp/x"},
		{name: "ignore file", path: ".syncthisignore"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := manifest.ValidRelPath(tc.path)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("ValidRelPath(%q) = %v, want nil", tc.path, err)
			case !tc.ok && err == nil:
				t.Fatalf("ValidRelPath(%q) = nil, want an error", tc.path)
			case !tc.ok && !errors.Is(err, manifest.ErrInvalidPath):
				t.Fatalf("ValidRelPath(%q) = %v, want ErrInvalidPath", tc.path, err)
			}
		})
	}
}

func TestValidLinkTarget(t *testing.T) {
	cases := []struct {
		name   string
		link   string
		target string
		ok     bool
	}{
		{name: "sibling", link: "a/link", target: "file.txt", ok: true},
		{name: "child", link: "link", target: "dir/file.txt", ok: true},
		{name: "up and back in", link: "a/b/link", target: "../c/file.txt", ok: true},
		{name: "own directory", link: "a/link", target: ".", ok: true},
		{name: "empty"},
		{name: "absolute", link: "a", target: "/etc"},
		{name: "root", link: "a", target: "/"},
		{name: "climbs out", link: "a", target: "../outside"},
		{name: "climbs out from a subdirectory", link: "a/b", target: "../../outside"},
		{name: "reaches the state directory", link: "a", target: ".syncthis"},
		{name: "nul byte", link: "a", target: "x\x00y"},
		{name: "invalid link path", link: "../a", target: "file.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := manifest.ValidLinkTarget(tc.link, tc.target)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("ValidLinkTarget(%q, %q) = %v, want nil", tc.link, tc.target, err)
			case !tc.ok && err == nil:
				t.Fatalf("ValidLinkTarget(%q, %q) = nil, want an error", tc.link, tc.target)
			}
		})
	}
}
