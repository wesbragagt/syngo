package syncer_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/syncer"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
)

// newLimitedDest returns a receiving device whose downloads are paced by
// limiter. One file at a time keeps the order of the calls readable.
func newLimitedDest(t *testing.T, handler ports.PeerHandler, limiter ports.Limiter) (*dest, *fakes.PeerConn) {
	t.Helper()
	d, conn := newDest(t, handler)
	d.puller = syncer.NewPuller(d.store, d.fs, d.hasher, d.clock, fakes.NewIdentity(destID, "dest"), d.events,
		syncer.PullerConfig{ChunkSize: chunkSize, Concurrency: 1, Download: limiter}, nil)
	return d, conn
}

// paced serves chunks but fails the request when the budget for it was not
// asked for first, which is what makes "the wait comes before the request"
// an assertion rather than an intention.
type paced struct {
	ports.PeerHandler
	limiter *fakes.Limiter

	mu     sync.Mutex
	served int
}

// Chunk refuses to serve a chunk the limiter has not been asked about.
func (p *paced) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	p.mu.Lock()
	p.served++
	nth := p.served
	p.mu.Unlock()

	if waits := len(p.limiter.Waits()); waits < nth {
		return ports.ChunkData{}, fmt.Errorf("chunk %d requested after %d waits, want at least %d",
			req.Index, waits, nth)
	}
	return p.PeerHandler.Chunk(ctx, peer, req)
}

func TestPull_WaitsForEveryChunkOnTheDownloadLimiter(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*3+17, 9)
	src.addFile(t, "big.bin", data)

	limiter := fakes.NewLimiter()
	d, conn := newLimitedDest(t, &paced{PeerHandler: src.server, limiter: limiter}, limiter)

	summary := d.sync(t, conn)
	if summary.Pulled != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want one pulled file", summary)
	}

	waits := limiter.Waits()
	if want := len(src.chunkList(t, "big.bin")); len(waits) != want {
		t.Fatalf("%d waits, want one per chunk (%d)", len(waits), want)
	}
	total := 0
	for _, n := range waits {
		total += n
	}
	if total != len(data) {
		t.Fatalf("waited for %d bytes, want the file size %d", total, len(data))
	}
}

func TestPull_NilLimiterNeverWaits(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize+5, 3)
	src.addFile(t, "big.bin", data)

	// The nil limiter is the default: newDest builds a puller without one.
	d, conn := newDest(t, src.server)
	if summary := d.sync(t, conn); summary.Pulled != 1 {
		t.Fatalf("summary = %+v, want one pulled file", summary)
	}
	if got := d.content(t, "big.bin"); string(got) != string(data) {
		t.Fatalf("pulled %d bytes, want %d", len(got), len(data))
	}
}

func TestChunk_WaitsForItsLengthOnTheUploadLimiter(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*4+9, 4)
	entry := src.addFile(t, "big.bin", data)

	chunks := src.chunkList(t, "big.bin")
	if len(chunks) < 2 {
		t.Fatalf("the fixture is %d chunks, the test needs several", len(chunks))
	}
	limiter := fakes.NewLimiter()
	src.server.SetUploadLimiter(limiter)

	var want []int
	for _, c := range chunks {
		served, err := src.chunkOf(context.Background(), "big.bin", entry.Hash, c.Index)
		if err != nil {
			t.Fatalf("chunk %d: %v", c.Index, err)
		}
		want = append(want, len(served.Data))
	}

	got := limiter.Waits()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want one per chunk %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits = %v, want one per chunk of its own length %v", got, want)
		}
	}
}

// TestPull_CancelledWaitLeavesTransferPending stops a transfer while it is
// waiting for its byte budget: the journal survives, nothing is reported as
// failed, and the transfer is picked up again the way a restart does it.
func TestPull_CancelledWaitLeavesTransferPending(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*2, 5)
	src.addFile(t, "big.bin", data)

	limiter := fakes.NewLimiter()
	release := limiter.Block()
	defer release()
	d, conn := newLimitedDest(t, src.server, limiter)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.puller.SyncFolder(ctx, conn, d.folder)
		done <- err
	}()

	<-limiter.Waited()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("sync returned nil, want the cancellation")
	}

	if n := d.events.Count("transfer.failed"); n != 0 {
		t.Fatalf("%d transfer.failed events, want none: a cancelled wait is not a failure", n)
	}
	live := d.liveTransfer(t, "big.bin")
	if live.State == transfer.StateFailed || live.State == transfer.StateDone {
		t.Fatalf("transfer state = %s, want it still resumable", live.State)
	}

	// A restart is what turns an abandoned transfer back into a pending
	// one, and it must find this transfer.
	if err := d.puller.ResumeAll(context.Background(), d.folder); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	if got := d.liveTransfer(t, "big.bin"); got.State != transfer.StatePending {
		t.Fatalf("transfer state = %s after a restart, want pending", got.State)
	}
}
