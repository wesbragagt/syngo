package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// pauseSettle is how long a paused transfer is watched to confirm that it
// really stopped asking for chunks.
const pauseSettle = 500 * time.Millisecond

// transferOf returns the single transfer a node reports.
func transferOf(t *testing.T, n *node) status.Transfer {
	t.Helper()
	report := n.Status()
	if len(report.Transfers.Items) != 1 {
		t.Fatalf("%s reports %+v, want exactly one transfer", n.name, report.Transfers.Items)
	}
	return report.Transfers.Items[0]
}

func TestPause_HaltsTransferAndResumeCompletesIt(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	writeFile(t, a, "huge.bin", payload(hugeFileSize, 51))
	pair(t, a, b)

	if !b.Events.Wait("transfer.chunk_verified", 5, convergeTimeout) {
		t.Fatalf("the receiver verified %d chunks, want at least 5",
			b.Events.Count("transfer.chunk_verified"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.App.Control.Pause(ctx, folderID); err != nil {
		t.Fatalf("pause %s: %v", b.name, err)
	}

	// Within one chunk the receiver stops asking for more.
	waitFor(t, "the transfer to stop", convergeTimeout, func() bool {
		before := b.Events.Count("transfer.chunk_verified")
		time.Sleep(pauseSettle)
		return b.Events.Count("transfer.chunk_verified") == before
	})
	stopped := b.Events.Count("transfer.chunk_verified")

	paused := transferOf(t, b)
	if paused.State != "paused" {
		t.Fatalf("transfer state = %q, want paused", paused.State)
	}
	if paused.ChunksDone != stopped || paused.ChunksDone >= paused.ChunksTotal {
		t.Fatalf("chunks done = %d of %d after %d verified chunks",
			paused.ChunksDone, paused.ChunksTotal, stopped)
	}
	if report := b.Status(); report.Transfers.Paused != 1 || report.Folders[0].State != "paused" {
		t.Fatalf("status = %+v, want a paused folder and one paused transfer", report)
	}

	// The partial file and everything verified into it survive the pause.
	parts := partFiles(t, b.Root)
	if len(parts) != 1 {
		t.Fatalf("partial files = %v, want exactly one", parts)
	}
	if exists(t, b, "huge.bin") {
		t.Fatal("a paused transfer installed the file")
	}

	if err := b.App.Control.Resume(ctx, folderID); err != nil {
		t.Fatalf("resume %s: %v", b.name, err)
	}
	waitTreeEqual(t, a, b)

	// The resumed transfer asked for the missing chunks only, so every
	// chunk of the file was verified exactly once.
	total := transferChunks(hugeFileSize, testChunkSize)
	if verified := b.Events.Count("transfer.chunk_verified"); verified != total {
		t.Fatalf("%d chunks verified in total, want %d: verified chunks were fetched again",
			verified, total)
	}
	if parts := partFiles(t, b.Root); len(parts) != 0 {
		t.Fatalf("partial files left behind: %v", parts)
	}
	if report := b.Status(); len(report.Transfers.Items) != 0 {
		t.Fatalf("the journal still holds %+v", report.Transfers.Items)
	}
}

func TestSyncPlan_ListsThePendingPullsAndExits10(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	pair(t, a, b)

	// The receiver is paused, so the plan describes work that is really
	// still outstanding while the command runs.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.App.Control.Pause(ctx, folderID); err != nil {
		t.Fatalf("pause %s: %v", b.name, err)
	}
	writeFile(t, a, "sub/one.txt", []byte("one"))
	writeFile(t, a, "two.txt", []byte("two"))
	waitFor(t, "the sender to scan its files", convergeTimeout, func() bool {
		return a.Status().Folders[0].Files == 2
	})

	code, stdout := runCLI(t, b.DataDir, "sync-plan", "docs")
	if code != 10 {
		t.Fatalf("sync-plan exit = %d, want 10 while files are missing\n%s", code, stdout)
	}
	for _, want := range []string{"mkdir   sub", "pull    sub/one.txt", "pull    two.txt", "missing-locally"} {
		if !contains(stdout, want) {
			t.Fatalf("sync-plan output does not mention %q:\n%s", want, stdout)
		}
	}

	if err := b.App.Control.Resume(ctx, folderID); err != nil {
		t.Fatalf("resume %s: %v", b.name, err)
	}
	waitTreeEqual(t, a, b)

	code, stdout = runCLI(t, b.DataDir, "sync-plan", "docs")
	if code != 0 {
		t.Fatalf("sync-plan exit = %d after convergence, want 0\n%s", code, stdout)
	}
	if !contains(stdout, "nothing would change") {
		t.Fatalf("sync-plan output = %q, want it to report an in-sync folder", stdout)
	}
}
