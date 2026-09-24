package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/daemon"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
)

// The two benchmarks in this file measure Phase 5 claims that need neither
// tens of gigabytes nor minutes of wall time, so they run in `make test`
// alongside the rest of the suite and their numbers are recorded in
// docs/BENCH.md.
//
// Both configure the daemon with the timings a shipped daemon runs at rather
// than the short ones the functional tests share: a convergence time measured
// with a 50 ms debounce is a measurement of the test harness, not of the
// product. The only setting they change from the default is the rescan
// interval, which is left at the shipped ten minutes on purpose so that
// nothing here can be carried by a periodic full scan: every change has to
// travel the watcher, scan, notify and pull path within the window being
// measured.

// benchReload makes both daemons re-read their device tables, which is what
// `peer add` asks of a running daemon. These benchmarks leave the rescan
// interval at the shipped ten minutes, and it is that ticker that would
// otherwise pick up a device paired after the daemon started.
func benchReload(t *testing.T, nodes ...*node) {
	t.Helper()
	for _, n := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
		err := n.App.Control.Reload(ctx)
		cancel()
		if err != nil {
			t.Fatalf("reload %s: %v", n.name, err)
		}
	}
}

// benchNodeConfig is a node configured the way a shipped daemon is: the
// default debounce, sync interval, rescan interval and chunk size.
func benchNodeConfig(mode folder.Mode) nodeConfig {
	return nodeConfig{
		Mode:           mode,
		ChunkSize:      transfer.DefaultChunkSize,
		Debounce:       daemon.DefaultDebounce,
		SyncInterval:   daemon.DefaultSyncInterval,
		RescanInterval: daemon.DefaultRescanInterval,
	}
}

// The document corpus the convergence benchmark edits: enough files that the
// folder is not trivially small, each the size of an ordinary note.
const (
	benchDocCount = 200
	benchDocSize  = 4 << 10
	// benchEditSize is the size of the document that is saved on each
	// round, an ordinary document rather than a note.
	benchEditSize = 24 << 10
	// benchEditRounds is how many saves are timed. One save measures
	// whatever the scheduler was doing at that instant; the reported
	// figures are the median and the slowest of this many.
	benchEditRounds = 10
	// benchConvergenceTarget is the Phase 5 claim.
	benchConvergenceTarget = time.Second
)

// TestBench_DocumentChangeConvergence measures how long an ordinary document
// save takes to appear, byte for byte, on the other device.
//
// What this is and is not: it is two real daemons with the shipped timings
// over loopback. The claim in .specs/tasks.md is about a healthy Tailscale
// direct path, which is this path plus a tailnet round trip and WireGuard on
// both ends, and which needs two machines on a tailnet to measure. Loopback
// is therefore a lower bound: a figure above one second here would rule the
// claim out, and a figure below it leaves the claim unproven until a
// two-machine run measures it. `syngo-bench --scenario tailscale-direct`
// is the run that does that.
func TestBench_DocumentChangeConvergence(t *testing.T) {
	a := startNodeWith(t, "a", benchNodeConfig(folder.ModeSendReceive))
	b := startNodeWith(t, "b", benchNodeConfig(folder.ModeSendReceive))

	for i := range benchDocCount {
		writeFile(t, a, fmt.Sprintf("notes/note-%03d.md", i), payload(benchDocSize, byte(i)))
	}
	pair(t, a, b)
	benchReload(t, a, b)
	waitTreeEqual(t, a, b)

	took := make([]time.Duration, 0, benchEditRounds)
	for round := range benchEditRounds {
		rel := fmt.Sprintf("notes/note-%03d.md", round)
		want := payload(benchEditSize, byte(100+round))
		full := filepath.Join(a.Root, rel)

		// An editor save: write a sibling temporary file and rename it
		// over the target, which is what Vim and Obsidian do and what
		// the watcher actually sees in normal use.
		started := time.Now()
		if err := os.WriteFile(full+".swp", want, 0o644); err != nil {
			t.Fatalf("write the temporary file for %s: %v", rel, err)
		}
		if err := os.Rename(full+".swp", full); err != nil {
			t.Fatalf("save %s: %v", rel, err)
		}
		waitFileEqual(t, b, rel, want)
		took = append(took, time.Since(started))
	}

	sorted := slices.Clone(took)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	slowest := sorted[len(sorted)-1]
	t.Logf("%d document saves of %s over loopback with the shipped timings "+
		"(debounce %v, sync interval %v, rescan interval %v): median %v, slowest %v, all %v",
		benchEditRounds, humanBytes(benchEditSize), daemon.DefaultDebounce,
		daemon.DefaultSyncInterval, daemon.DefaultRescanInterval, median, slowest, took)

	if median >= benchConvergenceTarget {
		t.Fatalf("the median save converged in %v, want under %v: loopback is the fastest path "+
			"a Tailscale direct connection can be, so the claim cannot hold above this",
			median, benchConvergenceTarget)
	}
}

// The small-edit benchmark: an ordinary document inside a large file, edited
// in place, at the shipped chunk size.
const (
	// benchEditFileSize is the file that is edited. It is well above the
	// 1 MiB default chunk size, so a whole-file transfer and a chunked one
	// differ by orders of magnitude rather than by a rounding error.
	benchEditFileSize = 64 << 20
	// benchSmallEditSize is the edit: a few kilobytes rewritten in place,
	// the shape of a save that changes a paragraph.
	benchSmallEditSize = 4 << 10
	// benchSavingTarget is the Phase 5 claim: at least 90% fewer bytes
	// than moving the file again.
	benchSavingTarget = 0.90
)

// TestBench_SmallEditUsesFarFewerBytes measures how many payload bytes cross
// the network for a small edit to a large file, against the size of the file
// it edited. Both daemons run at the shipped 1 MiB chunk size, so the number
// is what a default installation moves.
func TestBench_SmallEditUsesFarFewerBytes(t *testing.T) {
	a := startNodeWith(t, "a", benchNodeConfig(folder.ModeSendOnly))
	b := startNodeWith(t, "b", benchNodeConfig(folder.ModeReceiveOnly))

	original := payload(benchEditFileSize, 11)
	writeFile(t, a, "doc.bin", original)
	pair(t, a, b)
	benchReload(t, a, b)
	waitTreeEqual(t, a, b)

	whole, _ := chunkSourceBytes(t, b, "doc.bin")
	if whole < benchEditFileSize {
		t.Fatalf("the first copy moved %d bytes of %d, want the whole file", whole, benchEditFileSize)
	}

	// The edit: benchSmallEditSize bytes rewritten in the middle, in
	// place, so the file keeps its length and every chunk boundary but the
	// one covering the edit stays where it was.
	edited := slices.Clone(original)
	copy(edited[benchEditFileSize/2:], payload(benchSmallEditSize, 12))
	writeFile(t, a, "doc.bin", edited)
	waitFileEqual(t, b, "doc.bin", edited)

	after, _ := chunkSourceBytes(t, b, "doc.bin")
	moved := after - whole
	saving := 1 - float64(moved)/float64(benchEditFileSize)
	t.Logf("a %s edit to a %s file at the shipped %s chunk size moved %s over the network, "+
		"%.2f%% fewer bytes than the %s a whole-file transfer moves",
		humanBytes(benchSmallEditSize), humanBytes(benchEditFileSize),
		humanBytes(transfer.DefaultChunkSize), humanBytes(moved), saving*100,
		humanBytes(benchEditFileSize))

	if saving < benchSavingTarget {
		t.Fatalf("the edit moved %s of a %s file, a saving of %.2f%%, want at least %.0f%%",
			humanBytes(moved), humanBytes(benchEditFileSize), saving*100, benchSavingTarget*100)
	}
}
