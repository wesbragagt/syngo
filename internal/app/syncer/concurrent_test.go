package syncer_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// counted wraps a peer handler and records how many calls are inside it at the
// same time, so a test can tell whether two cycles of one folder overlapped.
type counted struct {
	ports.PeerHandler
	mu      sync.Mutex
	now     int
	highest int
}

// enter records one more call in flight and returns the function that records
// its end. Every call is held briefly, so two cycles that may overlap do.
func (c *counted) enter() func() {
	c.mu.Lock()
	c.now++
	if c.now > c.highest {
		c.highest = c.now
	}
	c.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	return func() {
		c.mu.Lock()
		c.now--
		c.mu.Unlock()
	}
}

// peak returns the highest number of calls that were in flight together.
func (c *counted) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.highest
}

// Manifest counts the call and serves the wrapped handler's manifest.
func (c *counted) Manifest(ctx context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error) {
	defer c.enter()()
	return c.PeerHandler.Manifest(ctx, peer, id)
}

// Chunk counts the call and serves the wrapped handler's chunk.
func (c *counted) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	defer c.enter()()
	return c.PeerHandler.Chunk(ctx, peer, req)
}

// TestSync_TwoPeersOfOneFolderDoNotOverlap proves the cycles of one folder run
// one after another. Only one transfer per path may be live, so two cycles at
// once give up each other's transfers and neither finishes.
func TestSync_TwoPeersOfOneFolderDoNotOverlap(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize*2, 9)
	src.addFile(t, "shared.bin", data)
	handler := &counted{PeerHandler: src.server}

	d, first := newDest(t, handler)
	second := fakes.NewPeerConn("OTHERPEER", "laptop", handler, time.Millisecond)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, conn := range []ports.PeerConn{first, second} {
		wg.Add(1)
		go func(i int, conn ports.PeerConn) {
			defer wg.Done()
			_, errs[i] = d.puller.SyncFolder(context.Background(), conn, d.folder)
		}(i, conn)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if peak := handler.peak(); peak != 1 {
		t.Fatalf("%d calls were in flight together, want the folder synced one cycle at a time", peak)
	}
	if got := d.content(t, "shared.bin"); string(got) != string(data) {
		t.Fatalf("shared.bin has %d bytes, want %d", len(got), len(data))
	}
	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("%d transfers left behind: %+v", len(list), list)
	}
}

// blocking serves chunks only after the test lets go, so a cycle can be
// cancelled while a transfer is in flight.
type blocking struct {
	ports.PeerHandler
	reached chan struct{}
	release chan struct{}
}

// Chunk reports that it was reached and waits.
func (b *blocking) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	select {
	case b.reached <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return ports.ChunkData{}, ctx.Err()
	}
	return b.PeerHandler.Chunk(ctx, peer, req)
}

// TestSync_CancelledTransferIsNotRemembered proves the note that a transfer
// was re-verified does not outlive the transfer: a cancelled cycle would
// otherwise leave one behind for every file it was fetching.
func TestSync_CancelledTransferIsNotRemembered(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "big.bin", payload(chunkSize*2, 4))
	handler := &blocking{
		PeerHandler: src.server,
		reached:     make(chan struct{}, 1),
		release:     make(chan struct{}),
	}

	d, conn := newDest(t, handler)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.puller.SyncFolder(ctx, conn, d.folder)
		done <- err
	}()

	<-handler.reached
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the cancelled cycle returned no error")
	}
	close(handler.release)

	if got := d.puller.ResumedCount(); got != 0 {
		t.Fatalf("%d transfers still remembered, want none", got)
	}
}
