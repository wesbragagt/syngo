package fakes_test

import (
	"context"
	"io"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// The fake filesystem is the substitute every later milestone tests against,
// so its walk, rename and partial-read behaviour is pinned here.

func newTree(t *testing.T) *fakes.FileSystem {
	t.Helper()
	fsys := fakes.NewFileSystem()
	fsys.AddFile("/root/a.txt", []byte("hello"), 0o644)
	fsys.AddFile("/root/sub/b.txt", []byte("world"), 0o644)
	fsys.AddDir("/root/skipme", 0o755)
	fsys.AddFile("/root/skipme/c.txt", []byte("nope"), 0o644)
	fsys.AddSymlink("/root/link", "a.txt")
	return fsys
}

func TestFileSystem_WalkLexicalOrderAndSkipDir(t *testing.T) {
	fsys := newTree(t)

	var visited []string
	err := fsys.Walk(context.Background(), "/root", func(rel string, info ports.FileInfo) error {
		visited = append(visited, rel)
		if rel == "skipme" {
			return ports.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk error: %v", err)
	}

	want := []string{"", "a.txt", "link", "skipme", "sub", "sub/b.txt"}
	if len(visited) != len(want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("visited = %v, want %v", visited, want)
		}
	}
}

func TestFileSystem_LstatAndReadLink(t *testing.T) {
	fsys := newTree(t)

	info, err := fsys.Lstat("/root/link")
	if err != nil {
		t.Fatalf("Lstat error: %v", err)
	}
	if !info.IsSymlink() {
		t.Fatalf("Lstat(/root/link) mode = %v, want a symlink", info.Mode)
	}
	target, err := fsys.ReadLink("/root/link")
	if err != nil || target != "a.txt" {
		t.Fatalf("ReadLink = (%q, %v), want (\"a.txt\", nil)", target, err)
	}
}

func TestFileSystem_ReadAtShortReadAtEOF(t *testing.T) {
	fsys := newTree(t)

	got, err := fsys.ReadAt("/root/a.txt", 3, 100)
	if err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if string(got) != "lo" {
		t.Fatalf("ReadAt = %q, want %q", got, "lo")
	}
}

func TestFileSystem_TempFileRenameRecorded(t *testing.T) {
	fsys := fakes.NewFileSystem()
	fsys.AddDir("/root/.syncthis/tmp", 0o755)

	tmp, err := fsys.CreateTemp("/root/.syncthis/tmp", "*.part")
	if err != nil {
		t.Fatalf("CreateTemp error: %v", err)
	}
	if _, err := tmp.WriteAt([]byte("chunk"), 0); err != nil {
		t.Fatalf("WriteAt error: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if err := fsys.Rename(tmp.Name(), "/root/final.bin"); err != nil {
		t.Fatalf("Rename error: %v", err)
	}

	content, ok := fsys.Content("/root/final.bin")
	if !ok || string(content) != "chunk" {
		t.Fatalf("content = %q (found %v), want %q", content, ok, "chunk")
	}
	if renames := fsys.Renames(); len(renames) != 1 || renames[0][1] != "/root/final.bin" {
		t.Fatalf("Renames() = %v", renames)
	}
	if _, err := fsys.Open(tmp.Name()); err == nil {
		t.Fatal("temp file still readable after rename")
	}
}

func TestFileSystem_RemoveRefusesNonEmptyDir(t *testing.T) {
	fsys := newTree(t)

	if err := fsys.Remove("/root/sub"); err == nil {
		t.Fatal("Remove(/root/sub) = nil, want error for a non-empty directory")
	}
	if err := fsys.Remove("/root/sub/b.txt"); err != nil {
		t.Fatalf("Remove(file) error: %v", err)
	}
	if err := fsys.Remove("/root/sub"); err != nil {
		t.Fatalf("Remove(empty dir) error: %v", err)
	}
	if removes := fsys.Removes(); len(removes) != 2 {
		t.Fatalf("Removes() = %v, want 2 entries", removes)
	}
}

func TestFileSystem_OpenReturnsWholeFile(t *testing.T) {
	fsys := newTree(t)

	rc, err := fsys.Open("/root/sub/b.txt")
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(data) != "world" {
		t.Fatalf("contents = %q, want %q", data, "world")
	}
}
