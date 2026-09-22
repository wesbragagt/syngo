package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/cli"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// hugeFileSize is large enough that the receiver is interrupted in the middle
// of it.
const hugeFileSize = 20 << 20

// statusJSON runs `status --json` against a data directory without a daemon,
// which is how the CLI reports a stopped node.
func statusJSON(t *testing.T, dataDir string) status.Report {
	t.Helper()
	var stdout, stderr strings.Builder
	code := cli.Execute([]string{"--data-dir", dataDir, "status", "--json"},
		strings.NewReader(""), &stdout, &stderr)
	if code != cli.ExitOK {
		t.Fatalf("status --json exited %d: %s", code, stderr.String())
	}

	var report status.Report
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("status --json is not valid json: %v\n%s", err, stdout.String())
	}
	return report
}

// runCLI runs a syncthis command against a data directory and returns its
// exit code and standard output.
func runCLI(t *testing.T, dataDir string, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	full := append([]string{"--data-dir", dataDir}, args...)
	code := cli.Execute(full, strings.NewReader(""), &stdout, &stderr)
	if stderr.Len() > 0 {
		t.Logf("%v stderr: %s", args, stderr.String())
	}
	return code, stdout.String()
}

// contains reports whether output holds s.
func contains(output, s string) bool { return strings.Contains(output, s) }

// transferChunks returns how many chunks a file of the given size is split
// into.
func transferChunks(size, chunkSize int) int {
	return (size + chunkSize - 1) / chunkSize
}

// partFiles lists the partial downloads of a folder.
func partFiles(t *testing.T, root string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".syncthis", "tmp", "*.part"))
	if err != nil {
		t.Fatalf("list partial files: %v", err)
	}
	return matches
}

func TestResume_ReceiverRestartKeepsVerifiedChunks(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	writeFile(t, a, "huge.bin", payload(hugeFileSize, 17))
	beforeOnA := readTree(t, a.Root)
	pair(t, a, b)

	// Stop the receiver as soon as it has verified a few chunks, so the
	// transfer is interrupted in the middle.
	if !b.Events.Wait("transfer.chunk_verified", 10, convergeTimeout) {
		t.Fatalf("the receiver verified %d chunks, want at least 10",
			b.Events.Count("transfer.chunk_verified"))
	}
	b.Stop()

	parts := partFiles(t, b.Root)
	if len(parts) != 1 {
		t.Fatalf("partial files = %v, want exactly one", parts)
	}

	report := statusJSON(t, b.DataDir)
	if report.Daemon.Running {
		t.Fatal("status reports a running daemon after the node was stopped")
	}
	if len(report.Transfers.Items) != 1 {
		t.Fatalf("transfers = %+v, want the interrupted one", report.Transfers.Items)
	}
	interrupted := report.Transfers.Items[0]
	if interrupted.Path != "huge.bin" || interrupted.Direction != "in" {
		t.Fatalf("transfer = %+v, want the inbound huge.bin", interrupted)
	}
	if interrupted.ChunksDone <= 0 || interrupted.ChunksDone >= interrupted.ChunksTotal {
		t.Fatalf("chunks done = %d of %d, want a transfer that is neither empty nor finished",
			interrupted.ChunksDone, interrupted.ChunksTotal)
	}
	if interrupted.BytesDone != int64(interrupted.ChunksDone)*int64(interrupted.ChunkSize) {
		t.Fatalf("bytes done = %d, want %d verified chunks of %d bytes",
			interrupted.BytesDone, interrupted.ChunksDone, interrupted.ChunkSize)
	}
	if state := interrupted.State; state != "active" && state != "pending" {
		t.Fatalf("state = %q, want the transfer to be resumable", state)
	}

	// The receiver starts again and finishes the file.
	b.Restart()
	waitTreeEqual(t, a, b)

	refetched := b.Events.Count("transfer.chunk_verified")
	if want := interrupted.ChunksTotal - interrupted.ChunksDone; refetched != want {
		t.Fatalf("the second run verified %d chunks, want exactly the %d that were missing",
			refetched, want)
	}
	if parts := partFiles(t, b.Root); len(parts) != 0 {
		t.Fatalf("partial files left behind: %v", parts)
	}
	if done := statusJSON(t, b.DataDir); len(done.Transfers.Items) != 0 {
		t.Fatalf("the journal still holds %+v", done.Transfers.Items)
	}

	// The sender is send-only: nothing the receiver did may change it.
	afterOnA := readTree(t, a.Root)
	if len(afterOnA) != len(beforeOnA) {
		t.Fatalf("the sender has %d entries, had %d", len(afterOnA), len(beforeOnA))
	}
	for path, was := range beforeOnA {
		is, ok := afterOnA[path]
		if !ok || is.Hash != was.Hash {
			t.Fatalf("the sender changed at %s", path)
		}
	}
}

func TestResume_SenderRestartFinishesTheTransfer(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	writeFile(t, a, "huge.bin", payload(hugeFileSize, 23))
	pair(t, a, b)

	if !b.Events.Wait("transfer.chunk_verified", 10, convergeTimeout) {
		t.Fatalf("the receiver verified %d chunks, want at least 10",
			b.Events.Count("transfer.chunk_verified"))
	}
	a.Restart()

	waitTreeEqual(t, a, b)
	report := b.Status()
	if report.Transfers.Failed != 0 {
		t.Fatalf("transfers = %+v, want none failed after the sender came back", report.Transfers)
	}
	// A finished transfer leaves nothing behind: the journal is empty, not
	// merely free of rows that count as failures.
	if rows := transfersOf(t, b); len(rows) != 0 {
		t.Fatalf("%s still journals %+v, want an empty transfer list", b.name, rows)
	}
}

// transferRow is one row of `transfer list --json`.
type transferRow struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// transfersOf returns the download journal a node reports.
func transfersOf(t *testing.T, n *node) []transferRow {
	t.Helper()
	out := cliOK(t, n, "transfer", "list", "--json")
	var rows []transferRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("transfer list on %s is not json: %v\n%s", n.name, err, out)
	}
	return rows
}

func TestResume_MissingPartialFileStartsTheTransferOver(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)
	writeFile(t, a, "huge.bin", payload(hugeFileSize, 29))
	pair(t, a, b)

	if !b.Events.Wait("transfer.chunk_verified", 10, convergeTimeout) {
		t.Fatalf("the receiver verified %d chunks, want at least 10",
			b.Events.Count("transfer.chunk_verified"))
	}
	b.Stop()

	parts := partFiles(t, b.Root)
	if len(parts) != 1 {
		t.Fatalf("partial files = %v, want exactly one", parts)
	}
	if err := os.Remove(parts[0]); err != nil {
		t.Fatalf("remove partial file: %v", err)
	}

	// The transfer survives the loss of its partial file: it starts over
	// rather than installing half a file.
	b.Restart()
	waitTreeEqual(t, a, b)
}

// assertNoConflicts fails when a node saw a conflict, whether it recorded a
// row for it or only warned about it.
func assertNoConflicts(t *testing.T, n *node) {
	t.Helper()
	if got := n.Events.Count("sync.conflict"); got != 0 {
		t.Fatalf("%s reported %d conflicts, want none", n.name, got)
	}
	if got := n.Status().Conflicts.Unresolved; got != 0 {
		t.Fatalf("%s holds %d unresolved conflicts, want none", n.name, got)
	}
}

// TestResume_SenderRestartIsNotAConflict covers the sender of a file that dies
// in the middle of serving it. What comes back to it from the peer afterwards
// is its own version, not an independent edit, so neither side may call it a
// conflict.
func TestResume_SenderRestartIsNotAConflict(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	writeFile(t, a, "big.bin", payload(hugeFileSize, 31))
	pair(t, a, b)

	if !b.Events.Wait("transfer.chunk_verified", 10, convergeTimeout) {
		t.Fatalf("the receiver verified %d chunks, want at least 10",
			b.Events.Count("transfer.chunk_verified"))
	}
	a.Restart()

	waitTreeEqual(t, a, b)
	waitTreeEqual(t, b, a)
	assertNoConflicts(t, a)
	assertNoConflicts(t, b)
}
