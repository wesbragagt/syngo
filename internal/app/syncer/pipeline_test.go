package syncer_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/pipeline"
)

// killAfter returns a connection that serves n chunks and then cancels the
// sync, whatever order the parallel fetchers asked in. It is a process death
// in the middle of a download with several chunks in flight.
func killAfter(src *source, n int, cancel context.CancelFunc) *fakes.PeerConn {
	var (
		mu     sync.Mutex
		served int
	)
	return fakes.NewPeerConn(peerID, "nas", handlerFunc{
		manifest: src.server.Manifest,
		chunks:   src.server.Chunks,
		chunk: func(c context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
			mu.Lock()
			served++
			over := served > n
			mu.Unlock()
			if over {
				cancel()
				return ports.ChunkData{}, c.Err()
			}
			return src.server.Chunk(c, peer, req)
		},
	}, time.Millisecond)
}

// TestPipeline_KilledParallelPullsNeverCorruptTheFile is the concurrency-safety
// proof. One large file is pulled with eight chunks in flight and the process
// is killed at a different point of every cycle, sixty times over, each kill
// followed by a restart that forgets everything not in the journal.
//
// Two things are asserted after every kill, not only at the end. What the
// journal calls verified must really be in the partial file, byte for byte:
// that is the invariant a parallel writer could break and a sequential one
// could not. And once the file finally arrives it must hash to the source,
// with nothing left in the journal.
func TestPipeline_KilledParallelPullsNeverCorruptTheFile(t *testing.T) {
	const kills = 60

	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*40, 23)
	src.addFile(t, "big.bin", data)

	d, conn := newDestWith(t, src.server, pipeline.Ceiling)

	for kill := range kills {
		ctx, cancel := context.WithCancel(context.Background())
		// A different point of the download every time, so the kill
		// lands in every part of the file and between every pair of
		// steps a chunk goes through.
		_, _ = d.puller.SyncFolder(ctx, killAfter(src, kill%17+1, cancel), d.folder)
		cancel()

		assertJournalMatchesDisk(t, d, kill)
		// The restart is what makes this a process death rather than a
		// cancelled call: nothing the puller remembered survives, so
		// the next cycle has only the journal and the partial file.
		d.restart()
	}

	// The kills are over. The file arrives.
	waitUntilPulled(t, d, conn)

	if got := d.content(t, "big.bin"); string(got) != string(data) {
		t.Fatalf("big.bin is %d bytes, want %d, and it does not match the source", len(got), len(data))
	}
	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d transfers left behind after the file arrived: %+v", len(list), list)
	}
}

// assertJournalMatchesDisk checks the invariant a parallel download has to
// keep: every chunk the journal calls verified holds, in the partial file, the
// bytes that hash to what the journal recorded.
func assertJournalMatchesDisk(t *testing.T, d *dest, kill int) {
	t.Helper()
	rows, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("kill %d: list transfers: %v", kill, err)
	}
	for _, row := range rows {
		content, ok := d.fs.Content(row.TempPath)
		if !ok {
			// A transfer whose partial file is gone resets itself on
			// the next cycle; there is nothing to disagree with.
			continue
		}
		for _, c := range row.Chunks {
			if !c.Verified {
				continue
			}
			if c.Offset+int64(c.Length) > int64(len(content)) {
				t.Fatalf("kill %d: chunk %d is journalled verified at %d..%d, the partial file is %d bytes",
					kill, c.Index, c.Offset, c.Offset+int64(c.Length), len(content))
			}
			got := content[c.Offset : c.Offset+int64(c.Length)]
			if d.hasher.SumBytes(got) != c.Hash {
				t.Fatalf("kill %d: chunk %d is journalled verified but the partial file holds other bytes at %d",
					kill, c.Index, c.Offset)
			}
		}
	}
}

// waitUntilPulled runs cycles until the file is installed, so a test does not
// depend on one cycle finishing a download that many kills interrupted.
func waitUntilPulled(t *testing.T, d *dest, conn *fakes.PeerConn) {
	t.Helper()
	for range 20 {
		summary, err := d.puller.SyncFolder(context.Background(), conn, d.folder)
		if err != nil {
			t.Fatalf("finishing cycle: %v", err)
		}
		if summary.Failed != 0 {
			t.Fatalf("finishing cycle failed: %+v", summary)
		}
		if _, ok := d.fs.Content(d.folder.Path + "/big.bin"); ok {
			return
		}
	}
	t.Fatal("the file did not arrive after twenty cycles")
}

// TestPipeline_ParallelPullsAskForEveryChunkExactlyOnce proves completeness
// without order: the fetchers ask for every missing chunk, none twice, and the
// file that lands hashes to the source.
func TestPipeline_ParallelPullsAskForEveryChunkExactlyOnce(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*30, 24)
	src.addFile(t, "big.bin", data)

	d, conn := newDestWith(t, src.server, pipeline.Ceiling)
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the file pulled", summary)
	}

	want := src.indexesFrom(t, "big.bin", 0)
	seen := make(map[int]int, len(want))
	for _, req := range conn.ChunkRequests() {
		seen[req.Index]++
	}
	for _, idx := range want {
		switch seen[idx] {
		case 1:
		case 0:
			t.Fatalf("chunk %d was never asked for", idx)
		default:
			t.Fatalf("chunk %d was asked for %d times", idx, seen[idx])
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("%d distinct chunks asked for, the file has %d", len(seen), len(want))
	}
	if got := d.content(t, "big.bin"); string(got) != string(data) {
		t.Fatal("the pulled file does not match the source")
	}
}

// watching records how many chunk requests of one file were in flight together
// and how many bytes of chunk content were held at once.
type watching struct {
	ports.PeerHandler
	mu      sync.Mutex
	now     int
	highest int
	hold    time.Duration
}

// Chunk holds the answer briefly so requests that may overlap do.
func (w *watching) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	w.mu.Lock()
	w.now++
	w.highest = max(w.highest, w.now)
	w.mu.Unlock()
	time.Sleep(w.hold)
	w.mu.Lock()
	w.now--
	w.mu.Unlock()
	return w.PeerHandler.Chunk(ctx, peer, req)
}

// peak returns the highest number of chunk requests in flight together.
func (w *watching) peak() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.highest
}

// TestPipeline_PinnedToOneAsksForOneChunkAtATime proves the pin is real, which
// is what a constrained device and the tests written against a single order
// rely on.
func TestPipeline_PinnedToOneAsksForOneChunkAtATime(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*12, 25))
	handler := &watching{PeerHandler: src.server, hold: time.Millisecond}

	d, conn := newDestWith(t, handler, 1)
	d.sync(t, conn)

	if peak := handler.peak(); peak != 1 {
		t.Fatalf("%d chunk requests were in flight together, want one: the pipeline is pinned to one", peak)
	}
}

// TestPipeline_WidePipelineOverlapsRequests proves the other half: a pipeline
// pinned wide really does have several chunk requests outstanding.
func TestPipeline_WidePipelineOverlapsRequests(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*12, 26))
	handler := &watching{PeerHandler: src.server, hold: 5 * time.Millisecond}

	d, conn := newDestWith(t, handler, pipeline.Ceiling)
	d.sync(t, conn)

	if peak := handler.peak(); peak < 2 {
		t.Fatalf("%d chunk requests were in flight together, want more than one", peak)
	}
	if peak := handler.peak(); peak > pipeline.Ceiling {
		t.Fatalf("%d chunk requests were in flight together, the ceiling is %d", peak, pipeline.Ceiling)
	}
	t.Logf("peak of %d chunk requests in flight, the pin is %d", handler.peak(), pipeline.Ceiling)
}

// TestPipeline_BudgetBoundsWhatIsHeldInMemory is the backpressure proof. The
// budget is set to two chunks, so however wide the pipeline is pinned, no more
// than two chunks' worth of content can be in flight: a slow disk cannot be
// made to queue the whole file in memory.
func TestPipeline_BudgetBoundsWhatIsHeldInMemory(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*20, 27)
	src.addFile(t, "big.bin", data)

	handler := &watching{PeerHandler: src.server, hold: 2 * time.Millisecond}
	d, conn := newDestBudget(t, handler, pipeline.Ceiling, 2*chunkSize)
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the file pulled", summary)
	}

	// Every chunk of this fixture is at most chunkSize, so a budget of two
	// of them admits at most two requests at a time whatever the pin says.
	if peak := handler.peak(); peak > 2 {
		t.Fatalf("%d chunk requests were in flight together, the budget allows two", peak)
	}
	if got := d.content(t, "big.bin"); string(got) != string(data) {
		t.Fatal("the pulled file does not match the source")
	}
	t.Logf("pinned to %d, held to %d by a budget of two chunks", pipeline.Ceiling, handler.peak())
}

// TestPipeline_ATransferInterruptedInParallelResumesFromTheJournal proves a
// download that was cut with several chunks in flight asks only for what is
// still missing, rather than starting the file again.
func TestPipeline_ATransferInterruptedInParallelResumesFromTheJournal(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*20, 28)
	src.addFile(t, "big.bin", data)

	d, conn := newDestWith(t, src.server, pipeline.Ceiling)
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = d.puller.SyncFolder(ctx, killAfter(src, 6, cancel), d.folder)
	cancel()
	d.restart()

	live := d.liveTransfer(t, "big.bin")
	verified := len(live.Chunks) - len(live.MissingChunks())
	if verified == 0 || verified == len(live.Chunks) {
		t.Fatalf("%d of %d chunks verified, want the transfer cut in the middle", verified, len(live.Chunks))
	}
	missing := live.MissingChunks()

	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the file finished", summary)
	}
	// Order is not preserved, so the comparison is of sets: what matters
	// is that exactly the missing chunks were asked for.
	asked := chunkIndexes(conn)
	slices.Sort(asked)
	if !sameIndexes(asked, missing) {
		t.Fatalf("the resuming cycle asked for %v, want the %d chunks the journal was missing: %v",
			asked, len(missing), missing)
	}
	if got := d.content(t, "big.bin"); string(got) != string(data) {
		t.Fatal("the resumed file does not match the source")
	}
	t.Logf("cut with %d of %d chunks verified, resumed asking for exactly the other %d",
		verified, len(live.Chunks), len(missing))
}

// TestPipeline_FloorIsSequential states the floor as a test rather than as a
// comment: nothing can put the pipeline below the download it replaced.
func TestPipeline_FloorIsSequential(t *testing.T) {
	if pipeline.Floor != 1 {
		t.Fatalf("the floor is %d, want one outstanding request: the sequential download", pipeline.Floor)
	}
	if got := pipeline.Next(pipeline.Floor, 0, time.Hour); got != 1 {
		t.Fatalf("a wholly disk-bound floor moved to %d, want it held at one", got)
	}
}

// TestPipeline_SpeedOnALinkWithLatency measures what the pipeline is actually
// for. Over loopback a round trip costs nothing, so parallel chunks only buy
// the overlap of this device's hashing and writing with the peer's reading;
// on a real path they buy the round trip itself, which is the larger number by
// far. The peer here answers after a fixed delay, which is that round trip.
//
// The assertion is that the wide pipeline is at least three times faster, well
// under the eight the pin allows, so the test states a real gain without
// depending on a quiet machine to reach the theoretical one.
func TestPipeline_SpeedOnALinkWithLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const roundTrip = 3 * time.Millisecond

	sequential := timeLatentPull(t, 1, roundTrip)
	parallel := timeLatentPull(t, pipeline.Ceiling, roundTrip)

	speedup := sequential.Seconds() / parallel.Seconds()
	t.Logf("60 chunks at a %v round trip: one at a time %v, %d at a time %v, speedup %.2fx",
		roundTrip, sequential.Round(time.Millisecond), pipeline.Ceiling,
		parallel.Round(time.Millisecond), speedup)

	if speedup < 3 {
		t.Fatalf("speedup %.2fx, want at least 3x on a link where the round trip dominates", speedup)
	}
}

// timeLatentPull pulls one file of sixty chunks from a peer that answers after
// roundTrip, and returns how long the whole pull took.
func timeLatentPull(t *testing.T, chunkPipeline int, roundTrip time.Duration) time.Duration {
	t.Helper()
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*60, 29)
	src.addFile(t, "big.bin", data)

	d, conn := newDestWith(t, &watching{PeerHandler: src.server, hold: roundTrip}, chunkPipeline)
	start := time.Now()
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want the file pulled", summary)
	}
	elapsed := time.Since(start)

	if got := d.content(t, "big.bin"); string(got) != string(data) {
		t.Fatal("the pulled file does not match the source")
	}
	return elapsed
}
