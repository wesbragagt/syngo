package fixture_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syngo/internal/bench/fixture"
)

func writeSeedFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRename(t *testing.T) {
	dir := t.TempDir()
	writeSeedFile(t, dir, "a.txt", "hello")

	result, err := fixture.Rename(dir, "a.txt", "sub/b.txt")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if result != (fixture.MutationResult{Op: fixture.OpRename, Path: "sub/b.txt", OldPath: "a.txt", Size: 5}) {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("old path still exists: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sub", "b.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("renamed content: %q, err %v", got, err)
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	writeSeedFile(t, dir, "a.txt", "hello")

	result, err := fixture.Delete(dir, "a.txt")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result != (fixture.MutationResult{Op: fixture.OpDelete, Path: "a.txt"}) {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("file still exists: %v", err)
	}
}

func TestModifyIsDeterministic(t *testing.T) {
	dirA := t.TempDir()
	writeSeedFile(t, dirA, "a.txt", "original")
	dirB := t.TempDir()
	writeSeedFile(t, dirB, "a.txt", "original")

	resultA, err := fixture.Modify(dirA, "a.txt", 5, 20)
	if err != nil {
		t.Fatalf("Modify: %v", err)
	}
	resultB, err := fixture.Modify(dirB, "a.txt", 5, 20)
	if err != nil {
		t.Fatalf("Modify: %v", err)
	}
	if resultA != resultB {
		t.Fatalf("results differ: %+v vs %+v", resultA, resultB)
	}

	contentA, _ := os.ReadFile(filepath.Join(dirA, "a.txt"))
	contentB, _ := os.ReadFile(filepath.Join(dirB, "a.txt"))
	if !bytes.Equal(contentA, contentB) {
		t.Fatalf("content differs across identical seeds")
	}
	if len(contentA) != 20 {
		t.Fatalf("size: got %d, want 20", len(contentA))
	}
}

func TestEditorSaveReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	writeSeedFile(t, dir, "note.md", "original content")

	result, err := fixture.EditorSave(dir, "note.md", 3, 15)
	if err != nil {
		t.Fatalf("EditorSave: %v", err)
	}
	if result.Op != fixture.OpEditorSave || result.Path != "note.md" || result.Size != 15 {
		t.Fatalf("unexpected result: %+v", result)
	}

	got, err := os.ReadFile(filepath.Join(dir, "note.md"))
	if err != nil || len(got) != 15 {
		t.Fatalf("content: %q, err %v", got, err)
	}
	if string(got) == "original conten" {
		t.Fatalf("editor save did not change content")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected the temp file to be gone after rename, got %v", entries)
	}
}
