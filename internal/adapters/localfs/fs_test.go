package localfs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/localfs"
	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// tree builds a small directory tree and returns its root.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "sub"))
	mustMkdir(t, filepath.Join(root, "target"))
	mustWrite(t, filepath.Join(root, "a.txt"), "alpha")
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), "beta")
	mustWrite(t, filepath.Join(root, "target", "hidden.txt"), "gamma")
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return root
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWalk_SlashPathsLexicalNoFollow(t *testing.T) {
	root := tree(t)

	var seen []string
	err := localfs.New().Walk(context.Background(), root, func(rel string, info ports.FileInfo) error {
		seen = append(seen, rel)
		if rel == "link" && !info.IsSymlink() {
			t.Errorf("link reported as %v, want symlink", info.Mode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk error: %v", err)
	}

	want := []string{"", "a.txt", "link", "sub", "sub/b.txt", "target", "target/hidden.txt"}
	if len(seen) != len(want) {
		t.Fatalf("walked %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("walked %v, want %v", seen, want)
		}
	}
}

func TestWalk_SkipDir(t *testing.T) {
	root := tree(t)

	var seen []string
	err := localfs.New().Walk(context.Background(), root, func(rel string, info ports.FileInfo) error {
		if rel == "target" {
			return ports.SkipDir
		}
		seen = append(seen, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	for _, rel := range seen {
		if rel == "target/hidden.txt" {
			t.Fatalf("walked into skipped directory: %v", seen)
		}
	}
}

func TestReadAt_ShortReadAtEOF(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	mustWrite(t, path, "alpha")
	fsys := localfs.New()

	got, err := fsys.ReadAt(path, 3, 10)
	if err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if string(got) != "ha" {
		t.Fatalf("ReadAt = %q, want %q", got, "ha")
	}

	got, err = fsys.ReadAt(path, 0, 5)
	if err != nil || string(got) != "alpha" {
		t.Fatalf("ReadAt = %q, %v", got, err)
	}
}

func TestCreateTemp_Rename_Atomic(t *testing.T) {
	root := t.TempDir()
	fsys := localfs.New()

	tmp, err := fsys.CreateTemp(root, "part-*")
	if err != nil {
		t.Fatalf("CreateTemp error: %v", err)
	}
	if _, err := tmp.WriteAt([]byte("payload"), 0); err != nil {
		t.Fatalf("WriteAt error: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	final := filepath.Join(root, "final.bin")
	if err := fsys.Rename(name, final); err != nil {
		t.Fatalf("Rename error: %v", err)
	}
	content, err := os.ReadFile(final)
	if err != nil || string(content) != "payload" {
		t.Fatalf("final file = %q, %v", content, err)
	}

	reopened, err := fsys.OpenTemp(final)
	if err != nil {
		t.Fatalf("OpenTemp error: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	buf := make([]byte, 7)
	if _, err := reopened.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if string(buf) != "payload" {
		t.Fatalf("reopened content = %q", buf)
	}
}

func TestInodePopulated(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	mustWrite(t, path, "alpha")

	info, err := localfs.New().Lstat(path)
	if err != nil {
		t.Fatalf("Lstat error: %v", err)
	}
	if info.Inode == 0 {
		t.Fatal("Inode = 0, want the real inode number")
	}
	if info.Name != "a.txt" || info.Size != 5 {
		t.Fatalf("info = %+v", info)
	}
}

func TestChtimesChmodReadLinkRemove(t *testing.T) {
	root := t.TempDir()
	fsys := localfs.New()
	path := filepath.Join(root, "a.txt")
	mustWrite(t, path, "alpha")

	want := time.Unix(1700000000, 123456789).UTC()
	if err := fsys.Chtimes(path, want); err != nil {
		t.Fatalf("Chtimes error: %v", err)
	}
	if err := fsys.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod error: %v", err)
	}
	info, err := fsys.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat error: %v", err)
	}
	if !info.ModTime.Equal(want) {
		t.Fatalf("ModTime = %v, want %v", info.ModTime, want)
	}
	if info.Mode.Perm() != 0o600 {
		t.Fatalf("Mode = %v, want 0600", info.Mode.Perm())
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink("a.txt", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target, err := fsys.ReadLink(link)
	if err != nil || target != "a.txt" {
		t.Fatalf("ReadLink = %q, %v", target, err)
	}

	if err := fsys.Remove(link); err != nil {
		t.Fatalf("Remove error: %v", err)
	}
	if _, err := fsys.Lstat(link); err == nil {
		t.Fatal("link still exists after Remove")
	}
}

func TestErrors_NameTheOperationAndPathOnce(t *testing.T) {
	fsys := localfs.New()
	missing := filepath.Join(t.TempDir(), "missing")

	cases := map[string]error{
		"lstat":    errorOf(func() error { _, err := fsys.Lstat(missing); return err }),
		"open":     errorOf(func() error { _, err := fsys.Open(missing); return err }),
		"readlink": errorOf(func() error { _, err := fsys.ReadLink(missing); return err }),
		"remove":   fsys.Remove(missing),
		"chmod":    fsys.Chmod(missing, 0o600),
	}
	for op, err := range cases {
		if err == nil {
			t.Fatalf("%s(%s) = nil, want an error", op, missing)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s error %v, want os.ErrNotExist", op, err)
		}
		if got := strings.Count(err.Error(), missing); got != 1 {
			t.Fatalf("%s error %q names the path %d times, want once", op, err, got)
		}
		if got := strings.Count(err.Error(), op+" "); got != 1 {
			t.Fatalf("%s error %q names the operation %d times, want once", op, err, got)
		}
	}
}

// errorOf runs fn and returns its error, so calls returning a value too can be
// used in a table.
func errorOf(fn func() error) error { return fn() }
