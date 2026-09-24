package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// limitedChunkSize is the chunk size the bandwidth tests use: the production
// default, so the arithmetic is the one a user sees.
const limitedChunkSize = 1 << 20

// limitedSize is how much is transferred while a limit is in force: 20 MiB at
// 4 MiB/s is five seconds, less the 100 ms burst the bucket starts with.
const limitedSize = 20 << 20

// limitedRate is the rate both bandwidth tests pace their transfer at.
const limitedRate = bandwidth.Rate(4 << 20)

// waitFileEqual waits until a path inside a node's folder holds want, and
// returns how long that took.
func waitFileEqual(t *testing.T, n *node, rel string, want []byte) {
	t.Helper()
	full := filepath.Join(n.Root, rel)
	waitFor(t, rel+" to arrive complete on "+n.name, convergeTimeout, func() bool {
		got, err := os.ReadFile(full)
		return err == nil && bytes.Equal(got, want)
	})
}

// TestDownloadLimit_20MiBAt4MiBPerSecondTakesAboutFiveSeconds is the claim the
// CLI documentation makes, measured end to end. The lower bound is what the
// limit promises; the upper bound is loose because everything else on the
// machine can only make the transfer slower.
func TestDownloadLimit_20MiBAt4MiBPerSecondTakesAboutFiveSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, ChunkSize: limitedChunkSize})
	b := startNodeWith(t, "b", nodeConfig{
		Mode: folder.ModeSendReceive, ChunkSize: limitedChunkSize, DownloadLimit: limitedRate,
	})
	pair(t, a, b)

	data := payload(limitedSize, 11)
	started := time.Now()
	writeFile(t, a, "big.bin", data)
	waitFileEqual(t, b, "big.bin", data)
	elapsed := time.Since(started)

	if elapsed < 4500*time.Millisecond {
		t.Fatalf("20 MiB at 4 MiB/s arrived in %v, want at least 4.5s", elapsed)
	}
	if max := 5*time.Second + 10*time.Second*timeoutScale; elapsed > max {
		t.Fatalf("20 MiB at 4 MiB/s arrived in %v, want under %v", elapsed, max)
	}
	t.Logf("download limited transfer took %v", elapsed)
}

// TestUploadLimit_SenderSideHoldsToo pins the other direction: the sender
// paces what it serves, whatever the receiver is willing to take.
func TestUploadLimit_SenderSideHoldsToo(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	a := startNodeWith(t, "a", nodeConfig{
		Mode: folder.ModeSendReceive, ChunkSize: limitedChunkSize, UploadLimit: limitedRate,
	})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive, ChunkSize: limitedChunkSize})
	pair(t, a, b)

	data := payload(limitedSize, 13)
	started := time.Now()
	writeFile(t, a, "big.bin", data)
	waitFileEqual(t, b, "big.bin", data)
	elapsed := time.Since(started)

	if elapsed < 4500*time.Millisecond {
		t.Fatalf("20 MiB served at 4 MiB/s arrived in %v, want at least 4.5s", elapsed)
	}
	if max := 5*time.Second + 10*time.Second*timeoutScale; elapsed > max {
		t.Fatalf("20 MiB served at 4 MiB/s arrived in %v, want under %v", elapsed, max)
	}
	t.Logf("upload limited transfer took %v", elapsed)
}

// TestNoLimit_StatusReportsZero is the "0 means unlimited" assertion: a device
// with no limits configured says so, and moves bytes without waiting for
// anything.
func TestNoLimit_StatusReportsZero(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	pair(t, a, b)

	for _, n := range []*node{a, b} {
		report := n.Status()
		if report.Daemon.UploadLimit != 0 || report.Daemon.DownloadLimit != 0 {
			t.Fatalf("%s reports limits %d/%d, want 0 for unlimited",
				n.name, report.Daemon.UploadLimit, report.Daemon.DownloadLimit)
		}
	}

	data := payload(64<<10, 17)
	writeFile(t, a, "small.bin", data)
	waitFileEqual(t, b, "small.bin", data)
}
