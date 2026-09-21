package tcpwire_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/tcpwire"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// syncHandler answers manifest and chunk requests from fixed values.
type syncHandler struct {
	info     ports.HelloInfo
	manifest manifest.Manifest
	err      error
	chunk    ports.ChunkData
	chunkErr error
}

// Hello returns the configured Hello.
func (h *syncHandler) Hello(context.Context) ports.HelloInfo { return h.info }

// Manifest returns the configured manifest or error.
func (h *syncHandler) Manifest(_ context.Context, _ device.ID, id folder.ID) (manifest.Manifest, error) {
	if h.err != nil {
		return manifest.Manifest{}, fmt.Errorf("folder %q: %w", id, h.err)
	}
	return h.manifest, nil
}

// Chunk returns the configured chunk or error.
func (h *syncHandler) Chunk(context.Context, device.ID, ports.ChunkRequest) (ports.ChunkData, error) {
	if h.chunkErr != nil {
		return ports.ChunkData{}, h.chunkErr
	}
	return h.chunk, nil
}

// hashOf returns a recognizable hash for a label.
func hashOf(t *testing.T, label string) manifest.Hash {
	t.Helper()
	hex := ""
	for len(hex) < 64 {
		hex += fmt.Sprintf("%02x", label[len(hex)/2%len(label)])
	}
	h, err := manifest.ParseHash(hex[:64])
	if err != nil {
		t.Fatalf("parse hash: %v", err)
	}
	return h
}

// connectTo starts a listener with the handler and returns a connection to it.
func connectTo(t *testing.T, h *syncHandler) (ports.PeerConn, *tcpwire.Listener) {
	t.Helper()
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	h.info = server.hello()

	ln, err := tcpwire.Listen(server.cert, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.SetTimeouts(2*time.Second, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ln.Serve(ctx, h, (&lookups{allow: true}).fn()) }()

	conn, err := newDialer(client).Dial(context.Background(), ln.Addr(), server.id)
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve() = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return conn, ln
}

func TestRequestManifest_RoundTrip(t *testing.T) {
	m := manifest.New("docs")
	m.Sequence = 42
	m.Add(manifest.Entry{
		Path: "sub/ünïcode.txt", Kind: manifest.KindFile, Size: 1234,
		ModTime: time.Unix(0, 1700000000123456789).UTC(), Mode: 0o640,
		Hash: hashOf(t, "content"), ModifiedBy: "SOMEDEVICE", Sequence: 7,
	})
	when := time.Unix(0, 1699999999000000000).UTC()
	m.Add(manifest.Entry{Path: "dir", Kind: manifest.KindDir, Mode: 0o755, ModTime: when, Sequence: 8})
	m.Add(manifest.Entry{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "sub", ModTime: when, Sequence: 9})
	m.Add(manifest.Entry{Path: "gone.txt", Kind: manifest.KindFile, Deleted: true, ModTime: when, Sequence: 10})

	conn, _ := connectTo(t, &syncHandler{manifest: m})
	got, err := conn.RequestManifest(context.Background(), "docs")
	if err != nil {
		t.Fatalf("request manifest: %v", err)
	}

	if got.FolderID != "docs" || got.Sequence != 42 {
		t.Fatalf("manifest = %s at %d, want docs at 42", got.FolderID, got.Sequence)
	}
	if len(got.Entries) != len(m.Entries) {
		t.Fatalf("%d entries, want %d", len(got.Entries), len(m.Entries))
	}
	for path, want := range m.Entries {
		have, ok := got.Entries[path]
		if !ok {
			t.Fatalf("entry %q did not survive the round trip", path)
		}
		if have.Kind != want.Kind || have.Size != want.Size || have.Mode != want.Mode ||
			have.Hash != want.Hash || have.LinkTarget != want.LinkTarget ||
			have.Deleted != want.Deleted || have.ModifiedBy != want.ModifiedBy ||
			have.Sequence != want.Sequence || !have.ModTime.Equal(want.ModTime) {
			t.Fatalf("entry %q = %+v, want %+v", path, have, want)
		}
	}
}

func TestRequestManifest_RefusalKeepsTheConnectionUsable(t *testing.T) {
	conn, _ := connectTo(t, &syncHandler{err: ports.ErrFolderReceiveOnly})

	_, err := conn.RequestManifest(context.Background(), "docs")
	if !errors.Is(err, ports.ErrFolderReceiveOnly) {
		t.Fatalf("error = %v, want ErrFolderReceiveOnly", err)
	}
	if _, err := conn.Ping(context.Background()); err != nil {
		t.Fatalf("ping after a refused manifest: %v", err)
	}
}

func TestRequestChunk_RoundTrip(t *testing.T) {
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i)
	}
	want := ports.ChunkData{Index: 3, Offset: 12288, Data: payload, ChunkHash: hashOf(t, "chunk")}
	conn, _ := connectTo(t, &syncHandler{chunk: want})

	got, err := conn.RequestChunk(context.Background(), ports.ChunkRequest{
		FolderID: "docs", Path: "big.bin", Hash: hashOf(t, "content"), Index: 3, ChunkSize: 4096,
	})
	if err != nil {
		t.Fatalf("request chunk: %v", err)
	}
	if got.Index != want.Index || got.Offset != want.Offset || got.ChunkHash != want.ChunkHash {
		t.Fatalf("chunk = %+v, want index %d at %d", got, want.Index, want.Offset)
	}
	if string(got.Data) != string(payload) {
		t.Fatalf("payload of %d bytes did not survive the round trip", len(payload))
	}
}

func TestRequestChunk_RefusalCarriesTheReason(t *testing.T) {
	conn, _ := connectTo(t, &syncHandler{chunkErr: &ports.ChunkError{
		FolderID: "docs", Path: "big.bin", Index: 2, Reason: ports.ChunkHashChanged,
	}})

	_, err := conn.RequestChunk(context.Background(), ports.ChunkRequest{
		FolderID: "docs", Path: "big.bin", Index: 2, ChunkSize: 4096,
	})
	var refused *ports.ChunkError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want a chunk error", err)
	}
	if refused.Reason != ports.ChunkHashChanged || refused.Path != "big.bin" || refused.Index != 2 {
		t.Fatalf("refusal = %+v, want hash_changed for big.bin chunk 2", refused)
	}
	if _, err := conn.Ping(context.Background()); err != nil {
		t.Fatalf("ping after a refused chunk: %v", err)
	}
}

func TestBroadcast_ReachesTheConnectedPeer(t *testing.T) {
	conn, ln := connectTo(t, &syncHandler{manifest: manifest.New("docs")})

	// The first request proves the connection is established before the
	// broadcast is sent.
	if _, err := conn.RequestManifest(context.Background(), "docs"); err != nil {
		t.Fatalf("request manifest: %v", err)
	}
	ln.Broadcast("docs", 17)

	select {
	case id := <-conn.Notifications():
		if id != "docs" {
			t.Fatalf("notified about %q, want docs", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no folder change notification arrived")
	}
}

func TestRequestManifest_RefusalReportsTheReasonOnce(t *testing.T) {
	conn, _ := connectTo(t, &syncHandler{err: ports.ErrFolderReceiveOnly})

	_, err := conn.RequestManifest(context.Background(), "docs")
	if !errors.Is(err, ports.ErrFolderReceiveOnly) {
		t.Fatalf("error = %v, want ErrFolderReceiveOnly", err)
	}
	// The peer's message already names the reason, so the client does not
	// add it a second time.
	if got, want := err.Error(), `folder "docs": folder is receive-only`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestAnnounceFolders_ReplacesWhatTheHelloAnnounced(t *testing.T) {
	conn, ln := connectTo(t, &syncHandler{manifest: manifest.New("docs")})

	// The first request proves the connection is established before the
	// announcement is sent.
	if _, err := conn.RequestManifest(context.Background(), "docs"); err != nil {
		t.Fatalf("request manifest: %v", err)
	}
	ln.AnnounceFolders([]ports.FolderAdvert{
		{ID: "docs", Mode: folder.ModeSendOnly},
		{ID: "pics", Mode: folder.ModeSendReceive},
	})

	// An empty id asks for a cycle over every folder both sides share,
	// which is what a new folder list calls for.
	select {
	case id := <-conn.Notifications():
		if id != "" {
			t.Fatalf("notified about %q, want every folder", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no folder announcement arrived")
	}

	got := conn.RemoteHello().Folders
	want := []ports.FolderAdvert{
		{ID: "docs", Mode: folder.ModeSendOnly},
		{ID: "pics", Mode: folder.ModeSendReceive},
	}
	if len(got) != len(want) {
		t.Fatalf("folders = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("folder %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The connection is still usable: the announcement is not a reply to
	// anything and must not disturb a request.
	if _, err := conn.RequestManifest(context.Background(), "docs"); err != nil {
		t.Fatalf("request manifest after the announcement: %v", err)
	}
}
