package tree_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/testutil/tree"
)

// write puts a file with content at rel under root, creating parents.
func write(t *testing.T, root, rel, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	// WriteFile honors the umask, so the mode is set again explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", rel, err)
	}
	return path
}

func read(t *testing.T, root string) map[string]tree.Entry {
	t.Helper()
	entries, err := tree.Read(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	return entries
}

func TestReadDescribesFilesDirsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dir/a.txt", "hello", 0o644)
	if err := os.Symlink("dir/a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := read(t, root)
	if len(got) != 3 {
		t.Fatalf("read %d entries, want 3: %v", len(got), got)
	}
	if e := got["dir"]; e.Kind != manifest.KindDir {
		t.Errorf("dir kind %s, want %s", e.Kind, manifest.KindDir)
	}
	file := got["dir/a.txt"]
	if file.Kind != manifest.KindFile || file.Size != 5 || file.Hash == "" {
		t.Errorf("dir/a.txt = %+v, want a 5 byte file with a hash", file)
	}
	if file.Mode != 0o644 {
		t.Errorf("dir/a.txt mode %v, want 644", file.Mode)
	}
	link := got["link"]
	if link.Kind != manifest.KindSymlink || link.Target != "dir/a.txt" {
		t.Errorf("link = %+v, want a symlink to dir/a.txt", link)
	}
}

func TestReadSkipsStateDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "a", 0o644)
	write(t, root, ".syngo/tmp/a.txt.part", "junk", 0o644)

	got := read(t, root)
	for path := range got {
		if path != "a.txt" {
			t.Errorf("read %q, want only a.txt", path)
		}
	}
}

func TestReadToleratesAPathRemovedMidWalk(t *testing.T) {
	root := t.TempDir()
	// The walk sorts by name, so removing "b" while "a" is being described
	// is what a daemon deleting a file under the reader looks like.
	write(t, root, "a.txt", "a", 0o644)
	gone := write(t, root, "b.txt", "b", 0o644)
	write(t, root, "c.txt", "c", 0o644)

	// os.RemoveAll of the whole directory tree would end the walk; removing
	// one entry after the walk has listed the directory is the race.
	dir, err := os.Open(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil || len(names) != 3 {
		t.Fatalf("readdirnames = %v, %v", names, err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove b.txt: %v", err)
	}

	got := read(t, root)
	if _, ok := got["b.txt"]; ok {
		t.Errorf("b.txt is present after removal: %+v", got["b.txt"])
	}
	if _, ok := got["a.txt"]; !ok {
		t.Errorf("a.txt is missing: %v", got)
	}
}

func TestReadOfAMissingRootIsEmpty(t *testing.T) {
	// The ErrNotExist tolerance that covers a path deleted mid-walk also
	// covers the root itself, so a root that is not there reads as an empty
	// folder rather than an error.
	got, err := tree.Read(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("read of a missing root: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("read of a missing root = %v, want empty", got)
	}
}

func TestDiffIsEmptyForEqualTrees(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		write(t, root, "dir/a.txt", "hello", 0o644)
	}
	if problems := tree.Diff(read(t, a), read(t, b)); len(problems) != 0 {
		t.Fatalf("diff of equal trees: %v", problems)
	}
}

func TestDiffReportsDifferences(t *testing.T) {
	now := time.Now()
	base := func() map[string]tree.Entry {
		return map[string]tree.Entry{
			"f": {Path: "f", Kind: manifest.KindFile, Size: 2, Mode: 0o644, Hash: "h1", ModTime: now},
		}
	}
	link := func(target string) map[string]tree.Entry {
		return map[string]tree.Entry{
			"l": {Path: "l", Kind: manifest.KindSymlink, Mode: 0o777, Target: target},
		}
	}

	cases := []struct {
		name string
		want map[string]tree.Entry
		got  map[string]tree.Entry
		says string
	}{
		{"missing", base(), map[string]tree.Entry{}, "missing: f"},
		{"unexpected", map[string]tree.Entry{}, base(), "unexpected: f"},
		{"kind", base(), map[string]tree.Entry{
			"f": {Path: "f", Kind: manifest.KindDir, Mode: 0o644, ModTime: now},
		}, "f: kind dir, want file"},
		{"hash", base(), map[string]tree.Entry{
			"f": {Path: "f", Kind: manifest.KindFile, Size: 2, Mode: 0o644, Hash: "h2", ModTime: now},
		}, "f: hash h2, want h1"},
		{"target", link("a"), link("b"), "l: link to b, want a"},
		{"mode", base(), map[string]tree.Entry{
			"f": {Path: "f", Kind: manifest.KindFile, Size: 2, Mode: 0o600, Hash: "h1", ModTime: now},
		}, "f: mode -rw-------, want -rw-r--r--"},
		{"mtime", base(), map[string]tree.Entry{
			"f": {Path: "f", Kind: manifest.KindFile, Size: 2, Mode: 0o644, Hash: "h1", ModTime: now.Add(-time.Hour)},
		}, "mtime"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			problems := tree.Diff(c.want, c.got)
			if len(problems) != 1 {
				t.Fatalf("diff = %v, want one problem", problems)
			}
			if !strings.Contains(problems[0], c.says) {
				t.Errorf("diff = %q, want it to mention %q", problems[0], c.says)
			}
		})
	}
}

// TestDiffIgnoresSmallMtimeAndSymlinkMode covers the two tolerances: a file
// whose mtime is off by less than a second is the same file, and a symlink's
// own mode is not something a sync can control.
func TestDiffIgnoresSmallMtimeAndSymlinkMode(t *testing.T) {
	now := time.Now()
	want := map[string]tree.Entry{
		"f": {Path: "f", Kind: manifest.KindFile, Mode: 0o644, Hash: "h", ModTime: now},
		"l": {Path: "l", Kind: manifest.KindSymlink, Mode: 0o777, Target: "f"},
	}
	got := map[string]tree.Entry{
		"f": {Path: "f", Kind: manifest.KindFile, Mode: 0o644, Hash: "h", ModTime: now.Add(500 * time.Millisecond)},
		"l": {Path: "l", Kind: manifest.KindSymlink, Mode: 0o755, Target: "f"},
	}
	if problems := tree.Diff(want, got); len(problems) != 0 {
		t.Fatalf("diff = %v, want none", problems)
	}
}
