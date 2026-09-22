package tcpwire_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/tcpwire"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// muxHandler answers a chunk request with bytes derived from its index, and
// records how many answers were being produced at the same time. A chunk is
// held briefly so requests that may overlap do.
type muxHandler struct {
	handler
	mu      sync.Mutex
	now     int
	highest int
	// hold is how long one answer takes.
	hold time.Duration
}

// Chunk answers with the index written into every byte.
func (h *muxHandler) Chunk(_ context.Context, _ device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	h.mu.Lock()
	h.now++
	h.highest = max(h.highest, h.now)
	h.mu.Unlock()
	time.Sleep(h.hold)
	h.mu.Lock()
	h.now--
	h.mu.Unlock()

	data := make([]byte, req.Length)
	for i := range data {
		data[i] = byte(req.Index)
	}
	// The chunk hash is the puller's business, not the wire's: this test is
	// about which reply reaches which caller, so a zero hash is enough.
	return ports.ChunkData{Index: req.Index, Offset: req.Offset, Data: data,
		ChunkHash: manifest.Hash{}}, nil
}

// peak returns the highest number of answers that were in flight together.
func (h *muxHandler) peak() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.highest
}

// TestMux_ConcurrentRequestsOnOneConnection proves the two halves of the
// request id: a client may have several requests outstanding on one
// connection, and every reply reaches the caller that asked for it. Each chunk
// carries its own index in every byte, so a reply handed to the wrong caller
// is caught rather than merely suspected.
func TestMux_ConcurrentRequestsOnOneConnection(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	h := &muxHandler{handler: handler{info: server.hello("")}, hold: 20 * time.Millisecond}
	ln := startServerWith(t, server, h, (&lookups{allow: true}).fn(), 0)

	conn, err := newDialer(client).Dial(context.Background(), ln.Addr(), server.id)
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	const requests = 8
	var wg sync.WaitGroup
	errs := make([]error, requests)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data, err := conn.RequestChunk(context.Background(), ports.ChunkRequest{
				FolderID: "docs", Path: "a.bin", Index: i, Offset: int64(i * 16), Length: 16,
			})
			switch {
			case err != nil:
				errs[i] = err
			case len(data.Data) != 16:
				errs[i] = fmt.Errorf("%d bytes, want 16", len(data.Data))
			case data.Data[0] != byte(i):
				errs[i] = fmt.Errorf("chunk %d answered with the content of chunk %d", i, data.Data[0])
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if peak := h.peak(); peak < 2 {
		t.Fatalf("%d requests were answered together, want the connection to carry more than one", peak)
	}
	t.Logf("%d of %d requests were in flight together", h.peak(), requests)
}

// TestMux_BoundsConcurrentRequests proves one peer cannot make this device
// answer an unbounded number of its requests at once: the slots are what keeps
// the memory a single connection can occupy bounded.
func TestMux_BoundsConcurrentRequests(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	h := &muxHandler{handler: handler{info: server.hello("")}, hold: 50 * time.Millisecond}
	ln := startServerWith(t, server, h, (&lookups{allow: true}).fn(), 0)

	conn, err := newDialer(client).Dial(context.Background(), ln.Addr(), server.id)
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	requests := tcpwire.MaxConcurrentRequests * 3
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = conn.RequestChunk(context.Background(), ports.ChunkRequest{
				FolderID: "docs", Path: "a.bin", Index: i, Offset: int64(i * 16), Length: 16,
			})
		}(i)
	}
	wg.Wait()

	if peak := h.peak(); peak > tcpwire.MaxConcurrentRequests {
		t.Fatalf("%d requests were answered together, the limit is %d",
			peak, tcpwire.MaxConcurrentRequests)
	}
	t.Logf("peak of %d concurrent answers, the limit is %d", h.peak(), tcpwire.MaxConcurrentRequests)
}
