package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// modeOf returns the permission bits of a path inside a node's folder.
func modeOf(t *testing.T, n *node, rel string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(filepath.Join(n.Root, rel))
	if err != nil {
		t.Fatalf("stat %s on %s: %v", rel, n.name, err)
	}
	return info.Mode().Perm()
}

// waitMode waits until a path inside a node's folder has the given mode.
func waitMode(t *testing.T, n *node, rel string, want os.FileMode) {
	t.Helper()
	waitFor(t, rel+" on "+n.name+" to take its new mode", convergeTimeout, func() bool {
		info, err := os.Lstat(filepath.Join(n.Root, rel))
		return err == nil && info.Mode().Perm() == want
	})
}

func TestKindChange_FileBecomesADirectory(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	writeFile(t, a, "flat/f", []byte("a plain file to begin with"))
	writeFile(t, a, "flat/other.txt", []byte("untouched"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	// The sender turns the file into a directory holding a file.
	if err := os.Remove(filepath.Join(a.Root, "flat/f")); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	writeFile(t, a, "flat/f/inner.txt", []byte("what is inside now"))

	waitTreeEqual(t, a, b)

	info, err := os.Lstat(filepath.Join(b.Root, "flat/f"))
	if err != nil {
		t.Fatalf("stat flat/f on b: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("flat/f on b is %v, want a directory", info.Mode())
	}

	// The cycle is not wedged: what comes after the replacement arrives too.
	writeFile(t, a, "after.txt", []byte("the cycle went on"))
	waitPresent(t, b, "after.txt")
}

func TestKindChange_DirectoryBecomesAFile(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	if err := os.MkdirAll(filepath.Join(a.Root, "empty-dir"), 0o755); err != nil {
		t.Fatalf("create empty dir: %v", err)
	}
	writeFile(t, a, "full-dir/one.txt", []byte("one"))
	writeFile(t, a, "full-dir/two.txt", []byte("two"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	// Both directories become files, one of them after its children are
	// deleted upstream.
	if err := os.Remove(filepath.Join(a.Root, "empty-dir")); err != nil {
		t.Fatalf("remove empty dir: %v", err)
	}
	writeFile(t, a, "empty-dir", []byte("a file now"))
	if err := os.RemoveAll(filepath.Join(a.Root, "full-dir")); err != nil {
		t.Fatalf("remove full dir: %v", err)
	}
	writeFile(t, a, "full-dir", []byte("a file now, and it was not empty"))

	waitTreeEqual(t, a, b)

	for _, rel := range []string{"empty-dir", "full-dir"} {
		info, err := os.Lstat(filepath.Join(b.Root, rel))
		if err != nil {
			t.Fatalf("stat %s on b: %v", rel, err)
		}
		if info.IsDir() {
			t.Fatalf("%s on b is still a directory", rel)
		}
	}
}

func TestModeChange_PropagatesWithoutTransferringContent(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "flat/file.txt", payload(4000, 3))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	before := readTree(t, b.Root)["flat/file.txt"]
	if err := os.Chmod(filepath.Join(a.Root, "flat/file.txt"), 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	waitMode(t, b, "flat/file.txt", 0o700)

	// Both sides settle on the new mode instead of handing it back and
	// forth, and the content never moved. The marker travels through the
	// same subtree, so its arrival means a cycle covered the file too.
	writeFile(t, a, "flat/marker.txt", []byte("a cycle ran"))
	waitPresent(t, b, "flat/marker.txt")
	if got := modeOf(t, a, "flat/file.txt"); got != 0o700 {
		t.Fatalf("mode on a = %v, want 0700", got)
	}
	if got := modeOf(t, b, "flat/file.txt"); got != 0o700 {
		t.Fatalf("mode on b = %v, want 0700", got)
	}
	after := readTree(t, b.Root)["flat/file.txt"]
	if after.Hash != before.Hash || after.Size != before.Size {
		t.Fatalf("file on b = %+v, want the content of %+v", after, before)
	}
}

func TestModeChange_IsNotAppliedToALocallyModifiedFile(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	writeFile(t, a, "flat/file.txt", []byte("the version both sides had"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	// The receiver edits its copy, then the sender changes the mode of its
	// own. The edit is the newer version, so nothing of the sender's is
	// applied to it.
	mine := []byte("the version the user of b is editing, and it is longer")
	writeFile(t, b, "flat/file.txt", mine)
	if err := os.Chmod(filepath.Join(a.Root, "flat/file.txt"), 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// The marker is written after the chmod and lives in the same subtree,
	// so the scan that picked it up picked the new mode up as well: b
	// pulling the marker means b saw the mode change and left it alone.
	writeFile(t, a, "flat/marker.txt", []byte("a cycle ran"))
	waitPresent(t, b, "flat/marker.txt")
	if got := modeOf(t, b, "flat/file.txt"); got != 0o644 {
		t.Fatalf("mode on b = %v, want the local 0644 kept", got)
	}
	got, err := os.ReadFile(filepath.Join(b.Root, "flat/file.txt"))
	if err != nil {
		t.Fatalf("read file on b: %v", err)
	}
	if string(got) != string(mine) {
		t.Fatalf("file on b = %q, want the local version", got)
	}
}
