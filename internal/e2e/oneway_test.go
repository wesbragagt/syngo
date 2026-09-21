package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// populate fills a folder with the tree the one-way tests synchronize: many
// small files in nested directories, a multi-chunk file, an empty file, an
// empty directory and a symlink.
func populate(t *testing.T, n *node) {
	t.Helper()
	for i := range 200 {
		dir := "flat"
		switch i % 4 {
		case 1:
			dir = "nested/one"
		case 2:
			dir = "nested/two/deeper"
		case 3:
			dir = "ünïcode ディレクトリ"
		}
		writeFile(t, n, filepath.Join(dir, fileName(i)), payload(64+i*7, byte(i)))
	}
	writeFile(t, n, "big.bin", payload(5<<20, 42))
	writeFile(t, n, "empty.txt", nil)

	if err := os.MkdirAll(filepath.Join(n.Root, "empty-dir"), 0o755); err != nil {
		t.Fatalf("create empty dir: %v", err)
	}
	if err := os.Symlink("big.bin", filepath.Join(n.Root, "link-to-big")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if err := os.Chmod(filepath.Join(n.Root, "flat", fileName(0)), 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

// fileName returns the name of the i-th generated file.
func fileName(i int) string {
	names := []string{"report", "notes", "データ", "a b c"}
	return names[i%len(names)] + "-" + itoa(i) + ".txt"
}

// itoa formats a small non-negative number.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestOneWay_SendOnlyFolderConverges(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	populate(t, a)
	pair(t, a, b)

	waitTreeEqual(t, a, b)

	t.Run("modify rename and delete propagate", func(t *testing.T) {
		before := readTree(t, a.Root)

		writeFile(t, a, filepath.Join("flat", fileName(4)), payload(9000, 99))
		from := filepath.Join(a.Root, "nested/one", fileName(1))
		to := filepath.Join(a.Root, "nested/one", "renamed.txt")
		if err := os.Rename(from, to); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := os.Remove(filepath.Join(a.Root, "flat", fileName(8))); err != nil {
			t.Fatalf("remove: %v", err)
		}

		waitTreeEqual(t, a, b)
		if _, err := os.Lstat(filepath.Join(b.Root, "flat", fileName(8))); !os.IsNotExist(err) {
			t.Fatalf("deleted file is still on b: %v", err)
		}
		if _, ok := before["nested/one/renamed.txt"]; ok {
			t.Fatal("the renamed file was already there before the rename")
		}
	})

	t.Run("the receiver never changes the sender", func(t *testing.T) {
		// b has everything a has, and b is receive-only: it must never
		// push anything back, and a must stay exactly as it is.
		before := readTree(t, a.Root)
		time.Sleep(3 * testSyncInterval)
		if problems := treeDiff(t, a.Root, a.Root); len(problems) != 0 {
			t.Fatalf("tree comparison is not stable: %v", problems)
		}
		after := readTree(t, a.Root)
		if len(before) != len(after) {
			t.Fatalf("a has %d entries, had %d", len(after), len(before))
		}
		for path, was := range before {
			is, ok := after[path]
			if !ok || is.Hash != was.Hash || is.Mode != was.Mode {
				t.Fatalf("a changed at %s: %+v, was %+v", path, is, was)
			}
		}

		// The sender refuses to receive, so its side of the transfer
		// journal stays empty.
		report := a.Status()
		if len(report.Transfers.Items) != 0 {
			t.Fatalf("the send-only node has transfers: %+v", report.Transfers.Items)
		}
	})

	t.Run("status reports the synchronized folder", func(t *testing.T) {
		report := b.Status()
		if len(report.Folders) != 1 {
			t.Fatalf("status lists %d folders, want 1", len(report.Folders))
		}
		if got := report.Folders[0].Files; got < 200 {
			t.Fatalf("status counts %d files, want at least 200", got)
		}
		if len(report.Peers) != 1 || !report.Peers[0].Connected {
			t.Fatalf("peers = %+v, want one connected peer", report.Peers)
		}
	})
}

func TestOneWay_TwoWayFolderConvergesBothWays(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "from-a.txt", payload(3000, 1))
	writeFile(t, b, "sub/from-b.txt", payload(70000, 2))
	pair(t, a, b)

	waitTreeEqual(t, a, b)
	waitTreeEqual(t, b, a)

	// Neither side re-sends what it received: once both are in sync the
	// trees stay put.
	time.Sleep(3 * testSyncInterval)
	if problems := treeDiff(t, a.Root, b.Root); len(problems) != 0 {
		t.Fatalf("the two folders drifted apart again:\n%s", join(problems))
	}

	transfers, err := b.App.Store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	for _, transfer := range transfers {
		t.Fatalf("transfer %s (%s) was left in the journal in state %s",
			transfer.ID, transfer.Path, transfer.State)
	}
}
