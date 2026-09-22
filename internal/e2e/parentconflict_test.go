package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// controlCtx is a context for one control call, cancelled when the test ends.
func controlCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	t.Cleanup(cancel)
	return c
}

// TestKindChange_AConflictedParentDoesNotStallTheFolder is D2: a turns a file
// into a directory while b edits the same file, so b keeps its file and can
// write nothing beneath it. That one path must not cost b the rest of its
// plan: an unrelated replacement made at the same time still completes and the
// transfer journal empties.
func TestKindChange_AConflictedParentDoesNotStallTheFolder(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "b.txt", []byte("the file both sides start with"))
	writeFile(t, a, "other", []byte("a file that becomes a directory"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	// Both changes are made while the two are apart, so b meets them in
	// one cycle: the conflicted kind change and an unrelated one.
	if err := b.App.Control.Pause(controlCtx(t), folderID); err != nil {
		t.Fatalf("pause b: %v", err)
	}
	writeFile(t, b, "b.txt", []byte("the version the user of b is editing"))
	waitFor(t, "b to scan its edit", convergeTimeout, func() bool {
		return b.Status().Folders[0].Files == 2
	})

	if err := os.Remove(filepath.Join(a.Root, "b.txt")); err != nil {
		t.Fatalf("remove b.txt on a: %v", err)
	}
	writeFile(t, a, "b.txt/x.txt", []byte("what is inside the new directory"))
	if err := os.Remove(filepath.Join(a.Root, "other")); err != nil {
		t.Fatalf("remove other on a: %v", err)
	}
	writeFile(t, a, "other/inner.txt", []byte("the replacement that must land"))
	waitFor(t, "a to scan the two kind changes", convergeTimeout, func() bool {
		return exists(t, a, "b.txt/x.txt") && a.Status().Folders[0].Files == 2
	})

	if err := b.App.Control.Resume(controlCtx(t), folderID); err != nil {
		t.Fatalf("resume b: %v", err)
	}

	// The unrelated replacement completes on b, which the blocked child
	// used to prevent by ending the cycle.
	waitPresent(t, b, "other/inner.txt")
	if got := readFileOf(t, b, "b.txt"); got != "the version the user of b is editing" {
		t.Fatalf("b.txt on b = %q, want the local edit kept", got)
	}

	// The journal empties: nothing is left half-transferred for the path
	// that cannot be written.
	waitFor(t, "the journal of b to empty", convergeTimeout, func() bool {
		return len(b.Status().Transfers.Items) == 0
	})
}

// readFileOf returns the contents of a path inside a node's folder.
func readFileOf(t *testing.T, n *node, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(n.Root, rel))
	if err != nil {
		t.Fatalf("read %s on %s: %v", rel, n.name, err)
	}
	return string(data)
}
