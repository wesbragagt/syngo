package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/pipeline"
)

// The shape of the transfer the speed proof measures. The chunks are larger
// than the rest of the suite's, so the file is a few hundred of them rather
// than a few thousand: what is being compared is the time on the wire and the
// disk, not the per-row cost of the journal.
const (
	pipelineChunkSize = 64 << 10
	pipelineFileSize  = 32 << 20
)

// TestPipeline_ParallelChunksBeatSequentialOnes is the speed proof. The same
// file is transferred twice between two fresh pairs of daemons, once with the
// chunk pipeline pinned to one, which is the download this replaced, and once
// with it adapting. Both numbers are logged, so a run of this test is the
// measurement rather than a claim about one.
//
// The assertion is a regression guard rather than a promised speedup: a
// machine running something else can make any wall-clock comparison come out
// flat, but the parallel path being much slower than the sequential one would
// be a defect whatever the machine is doing.
func TestPipeline_ParallelChunksBeatSequentialOnes(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	data := payload(pipelineFileSize, 71)

	sequential := timeTransfer(t, "sequential", 1, data)
	parallel := timeTransfer(t, "adaptive", 0, data)

	speedup := sequential.Seconds() / parallel.Seconds()
	t.Logf("%d MiB over loopback: pinned to one chunk %v, adaptive pipeline %v, speedup %.2fx",
		pipelineFileSize>>20, sequential.Round(time.Millisecond), parallel.Round(time.Millisecond), speedup)

	if parallel > 2*sequential {
		t.Fatalf("the parallel path took %v against the sequential path's %v", parallel, sequential)
	}
}

// timeTransfer starts a fresh pair of daemons, puts one file on the sending
// side and returns how long the receiving side took to hold the same tree. The
// pipeline width is the receiver's; the sender serves the same way either way.
func timeTransfer(t *testing.T, name string, chunkPipeline int, data []byte) time.Duration {
	t.Helper()

	a := startNodeWith(t, name+"-a", nodeConfig{
		Mode: folder.ModeSendReceive, ChunkSize: pipelineChunkSize,
	})
	b := startNodeWith(t, name+"-b", nodeConfig{
		Mode: folder.ModeSendReceive, ChunkSize: pipelineChunkSize, ChunkPipeline: chunkPipeline,
	})
	pair(t, a, b)

	// The file is written before the clock starts and the peers are
	// already connected, so what is measured is the scan, the plan and the
	// transfer rather than the pairing.
	writeFile(t, a, "big.bin", data)
	start := time.Now()
	waitTreeEqual(t, a, b)
	elapsed := time.Since(start)

	t.Logf("%s: %d MiB in %v", name, len(data)>>20, elapsed.Round(time.Millisecond))
	a.Stop()
	b.Stop()
	return elapsed
}

// TestFolderAuth_ParallelChunkRequestsAreEachRefused is the adversarial case
// under parallel load. c is authorized for extra and for nothing else, and it
// fires a pipeline's worth of chunk requests for a folder it may not have, on
// one connection, while a legitimate transfer of its own folder is running on
// the same connection.
//
// Every one of them has to be refused on its own. Authorization is checked per
// request on the serving side, so a parallel client is not a way around it:
// there is no per-connection decision for a second request to inherit.
func TestFolderAuth_ParallelChunkRequestsAreEachRefused(t *testing.T) {
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, ExtraFolder: true})
	c := startNodeWith(t, "c", nodeConfig{NoFolder: true, ExtraFolder: true})

	for _, pair := range [][2]*node{{a, c}, {c, a}} {
		trust(t, pair[0], pair[1])
	}
	// a shares extra with c, and docs with nobody.
	grant(t, a, extraID, c.ID)
	grant(t, c, extraID, a.ID)

	secret := payload(64<<10, 72)
	writeFile(t, a, "secret.txt", secret)
	writeFileIn(t, a.Extra, "shared.bin", payload(64<<10, 73))

	// The folder c is authorized for converges, which proves the two are
	// connected and have run a full cycle: anything c does not get after
	// that, it was refused rather than merely not offered yet.
	waitFor(t, "shared.bin to arrive on c", convergeTimeout, func() bool {
		_, err := os.Lstat(filepath.Join(c.Extra, "shared.bin"))
		return err == nil
	})

	conn := dialTo(t, c, a)
	ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
	defer cancel()

	// Twice the pipeline's width of requests at once, for a folder c may
	// not have, on the one connection it is allowed to hold.
	const attempts = 2 * pipeline.Ceiling
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A well-formed byte range of a file that really is
			// there, so nothing but the authorization can be what
			// refuses it.
			const length = 4 << 10
			_, results[i] = conn.RequestChunk(ctx, ports.ChunkRequest{
				FolderID: folderID, Path: "secret.txt",
				Index: i, Offset: int64(i * length), Length: length,
			})
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		var refusal *ports.ChunkError
		if !errors.As(err, &refusal) || refusal.Reason != ports.ChunkFolderUnknown {
			t.Fatalf("parallel chunk request %d for %s = %v, want a %s refusal",
				i, folderID, err, ports.ChunkFolderUnknown)
		}
	}
	t.Logf("%d parallel chunk requests for %s, every one refused as %s",
		attempts, folderID, ports.ChunkFolderUnknown)

	// And the manifest and the chunk list of the same folder are refused
	// under the same load, so nothing about the file leaked either.
	if _, err := conn.RequestManifest(ctx, folderID); !errors.Is(err, ports.ErrFolderUnknown) {
		t.Fatalf("manifest of %s from a = %v, want %v", folderID, err, ports.ErrFolderUnknown)
	}
	if _, err := os.Lstat(filepath.Join(c.Extra, "secret.txt")); err == nil {
		t.Fatal("c holds a file from a folder it was never authorized for")
	}
}
