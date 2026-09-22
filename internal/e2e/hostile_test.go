package e2e

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/blake3hash"
	"github.com/wesbragagt/syncthis/internal/adapters/identityfile"
	"github.com/wesbragagt/syncthis/internal/adapters/tcpwire"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// hostilePeer is a peer that speaks the protocol but answers with whatever a
// test tells it to, including manifests no honest sender would produce. It is
// the attacker in the path traversal tests: a paired device, so everything the
// receiver does with what it sends is the receiver's own decision.
type hostilePeer struct {
	t     *testing.T
	id    device.ID
	addr  string
	files map[string][]byte

	mu      sync.Mutex
	entries []manifest.Entry
}

// startHostilePeer binds a listener with a fresh identity and serves whatever
// SetManifest was last given.
func startHostilePeer(t *testing.T) *hostilePeer {
	t.Helper()
	identity, _, err := identityfile.LoadOrCreate(t.TempDir(), "hostile")
	if err != nil {
		t.Fatalf("hostile identity: %v", err)
	}
	listener, err := tcpwire.Listen(identity.TLSCertificate(), "127.0.0.1:0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("hostile listen: %v", err)
	}

	p := &hostilePeer{
		t: t, id: identity.DeviceID(), addr: listener.Addr(),
		files: make(map[string][]byte),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Every device may talk to the attacker; the receiver is the
		// side whose behaviour is under test.
		known := func(context.Context, device.ID) bool { return true }
		if err := listener.Serve(ctx, p, known); err != nil && ctx.Err() == nil {
			t.Errorf("hostile serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
	})
	return p
}

// SetManifest replaces what the peer answers manifest requests with.
func (p *hostilePeer) SetManifest(entries ...manifest.Entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append([]manifest.Entry(nil), entries...)
}

// Serve makes the peer offer a file's content, so a receiver that accepts the
// manifest can download it.
func (p *hostilePeer) Serve(rel string, data []byte) manifest.Entry {
	p.files[rel] = data
	hash := blake3hash.New().SumBytes(data)
	return manifest.Entry{
		Path: rel, Kind: manifest.KindFile, Size: int64(len(data)), Mode: 0o644,
		ModTime: time.Now().Truncate(time.Second), Hash: hash,
	}
}

// Hello announces the shared folder as a sending one.
func (p *hostilePeer) Hello(context.Context, device.ID) ports.HelloInfo {
	return ports.HelloInfo{
		DeviceID: p.id,
		Name:     "hostile",
		Folders:  []ports.FolderAdvert{{ID: folderID, Mode: folder.ModeSendReceive}},
	}
}

// Manifest answers with the entries the test set, unchanged.
func (p *hostilePeer) Manifest(_ context.Context, _ device.ID, id folder.ID) (manifest.Manifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := manifest.New(id)
	m.Sequence = uint64(len(p.entries) + 1)
	for _, e := range p.entries {
		m.Add(e)
	}
	return m, nil
}

// Chunk serves a piece of a file the peer offered.
func (p *hostilePeer) Chunk(_ context.Context, _ device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	data, ok := p.files[req.Path]
	if !ok {
		return ports.ChunkData{}, &ports.ChunkError{
			FolderID: req.FolderID, Path: req.Path, Index: req.Index, Reason: ports.ChunkNotFound,
		}
	}
	offset := int64(req.Index) * int64(req.ChunkSize)
	end := min(offset+int64(req.ChunkSize), int64(len(data)))
	piece := data[offset:end]
	return ports.ChunkData{
		Index: req.Index, Offset: offset, Data: piece, ChunkHash: blake3hash.New().SumBytes(piece),
	}, nil
}

// trust makes the receiver dial the hostile peer, as `peer add` would.
func (p *hostilePeer) trust(t *testing.T, n *node) {
	t.Helper()
	err := n.App.Store.SaveDevice(context.Background(), device.Device{
		ID: p.id, Name: "hostile", Address: p.addr, Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("trust the hostile peer: %v", err)
	}
}

// assertNotWritten fails when the file at p holds what the peer offered under
// the given entry, which would mean the receiver installed it there.
func assertNotWritten(t *testing.T, p string, offered manifest.Entry) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		return
	}
	if blake3hash.New().SumBytes(got) == offered.Hash {
		t.Fatalf("the hostile peer wrote %s", p)
	}
}

// TestHostilePeer_CannotWriteOutsideTheFolder is the proof for the path
// traversal report: a paired peer serves a manifest naming paths above the
// folder root, and nothing appears outside it. The peer then serves an honest
// file, which does arrive, so the test cannot pass by never syncing at all.
func TestHostilePeer_CannotWriteOutsideTheFolder(t *testing.T) {
	receiver := startNode(t, "receiver", folder.ModeSendReceive)
	peer := startHostilePeer(t)

	outside := filepath.Dir(receiver.Root)
	writeFileIn(t, outside, "victim.txt", []byte("this file belongs to the user"))

	escape := peer.Serve("../escape.txt", []byte("written outside the folder"))
	deep := peer.Serve("../../escape-deep.txt", []byte("written two levels out"))
	cleaned := peer.Serve("a/../../escape-clean.txt", []byte("written out through a clean"))
	absolute := peer.Serve("/tmp/syncthis-escape-absolute", []byte("written at an absolute path"))
	reserved := peer.Serve(".syncthisignore", []byte("*\n"))
	tombstone := manifest.Entry{Path: "../victim.txt", Kind: manifest.KindFile, Deleted: true}
	peer.SetManifest(escape, deep, cleaned, absolute, reserved, tombstone)
	peer.trust(t, receiver)

	// Give the receiver several sync cycles with the hostile manifest.
	time.Sleep(2 * time.Second * timeoutScale)

	// The content is compared rather than the mere existence: two of the
	// paths land in a directory the test does not own.
	assertNotWritten(t, filepath.Join(outside, "escape.txt"), escape)
	assertNotWritten(t, filepath.Join(filepath.Dir(outside), "escape-deep.txt"), deep)
	assertNotWritten(t, filepath.Join(filepath.Dir(outside), "escape-clean.txt"), cleaned)
	assertNotWritten(t, "/tmp/syncthis-escape-absolute", absolute)
	assertNotWritten(t, filepath.Join(receiver.Root, ".syncthisignore"), reserved)
	if _, err := os.Lstat(filepath.Join(outside, "victim.txt")); err != nil {
		t.Fatalf("the hostile peer deleted the file outside the folder: %v", err)
	}

	// The same peer, now honest, is served: the connection and the sync
	// cycle work, so the refusals above are decisions and not silence.
	honest := peer.Serve("honest.txt", []byte("an ordinary file"))
	peer.SetManifest(honest)
	waitFor(t, "the honest file to arrive", convergeTimeout, func() bool {
		_, err := os.Lstat(filepath.Join(receiver.Root, "honest.txt"))
		return err == nil
	})
}

// TestHostilePeer_CannotEscapeThroughASymlink is the proof for the symlink
// report: the peer offers a link pointing out of the folder and a file beneath
// it. Neither the link nor the file may be created.
func TestHostilePeer_CannotEscapeThroughASymlink(t *testing.T) {
	receiver := startNode(t, "receiver", folder.ModeSendReceive)
	peer := startHostilePeer(t)

	outside := t.TempDir()
	link := manifest.Entry{
		Path: "a", Kind: manifest.KindSymlink, LinkTarget: outside,
		Mode: 0o777, ModTime: time.Now().Truncate(time.Second),
	}
	child := peer.Serve("a/passwd", []byte("written through the link"))
	peer.SetManifest(link, child)
	peer.trust(t, receiver)

	time.Sleep(2 * time.Second * timeoutScale)

	assertNotWritten(t, filepath.Join(outside, "passwd"), child)
	if _, err := os.Lstat(filepath.Join(receiver.Root, "a")); err == nil {
		t.Fatal("the symlink leaving the folder was created")
	}

	honest := peer.Serve("honest.txt", []byte("an ordinary file"))
	peer.SetManifest(honest)
	waitFor(t, "the honest file to arrive", convergeTimeout, func() bool {
		_, err := os.Lstat(filepath.Join(receiver.Root, "honest.txt"))
		return err == nil
	})
}
