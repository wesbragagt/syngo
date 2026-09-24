package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// TestChmod_DoesNotEraseAConcurrentEdit is the chmod race: a mode change on
// one side and a content edit on the other, both made while the folders are
// paused, must not lose the edit. The edit is scanned while the other side's
// mode change is being applied, which is the window the manifest write has to
// survive.
func TestChmod_DoesNotEraseAConcurrentEdit(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "m.txt", []byte("one"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()
	if err := a.App.Control.Pause(ctx, folderID); err != nil {
		t.Fatalf("pause a: %v", err)
	}
	if err := b.App.Control.Pause(ctx, folderID); err != nil {
		t.Fatalf("pause b: %v", err)
	}

	if err := os.Chmod(filepath.Join(a.Root, "m.txt"), 0o755); err != nil {
		t.Fatalf("chmod on a: %v", err)
	}
	writeFile(t, b, "m.txt", []byte("two"))

	if err := a.App.Control.Resume(ctx, folderID); err != nil {
		t.Fatalf("resume a: %v", err)
	}
	if err := b.App.Control.Resume(ctx, folderID); err != nil {
		t.Fatalf("resume b: %v", err)
	}

	waitFor(t, "the edit made on b to reach a", convergeTimeout, func() bool {
		return holdsContent(t, a.Root, "two") && holdsContent(t, b.Root, "two")
	})
}

// holdsContent reports whether any file in the tree holds want.
func holdsContent(t *testing.T, root, want string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a tree being written under us is not a failure
		}
		if strings.Contains(filepath.ToSlash(p), "/.syngo/") {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr == nil && string(data) == want {
			found = true
		}
		return nil
	})
	return found
}
