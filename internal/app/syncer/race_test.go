package syncer_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// racingStore runs a local change once, right after the puller has loaded the
// manifest it plans against. It is the window in which a scan of a local edit
// lands between a cycle's plan and the writes that cycle makes.
type racingStore struct {
	*fakes.Store
	once sync.Once
	edit func()
}

// LoadManifest hands out the manifest and then lets the local change land.
func (s *racingStore) LoadManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error) {
	m, err := s.Store.LoadManifest(ctx, id)
	s.once.Do(s.edit)
	return m, err
}

// newRacingDest returns a destination whose store lets edit land after the
// plan was made, together with the store behind it.
func newRacingDest(t *testing.T, handler ports.PeerHandler, edit func(*dest)) (*dest, *fakes.PeerConn) {
	t.Helper()
	f := testFolder("docs", false)
	f.Path = "/home/dest/docs"

	d := &dest{
		store: fakes.NewStore(), fs: fakes.NewFileSystem(), hasher: fakes.NewHasher(),
		clock: fakes.NewClock(time.Time{}), events: fakes.NewEvents(), folder: f,
	}
	d.fs.AddDir(f.Path, 0o755)
	if err := d.store.SaveFolder(context.Background(), f); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	racing := &racingStore{Store: d.store, edit: func() { edit(d) }}
	d.puller = syncer.NewPuller(racing, d.fs, d.hasher, d.clock, fakes.NewIdentity(destID, "dest"),
		d.events, syncer.PullerConfig{ChunkSize: chunkSize, Concurrency: 2}, nil)
	return d, fakes.NewPeerConn(peerID, "nas", handler, time.Millisecond)
}

// TestSync_ChmodDoesNotEraseAnEditThatLandedAfterThePlan is D1: the peer only
// changed the permission bits, and an edit made here between the plan and the
// chmod must survive both on disk and in the manifest.
func TestSync_ChmodDoesNotEraseAnEditThatLandedAfterThePlan(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	shared := []byte("what both sides had")
	src.addFile(t, "a.txt", shared)
	src.chmod(t, "a.txt", 0o700)

	mine := []byte("the version the user just wrote")
	d, conn := newRacingDest(t, src.server, func(d *dest) {
		// What a scan of the local edit writes: the new content on disk
		// and the entry describing it, with this device's counter.
		d.record(t, "a.txt", mine, vector(t, "SOURCE:1,DEST:1"))
	})
	d.record(t, "a.txt", shared, vector(t, "SOURCE:1"))

	summary := d.sync(t, conn)
	if summary.Modes != 0 || summary.Failed != 1 {
		t.Fatalf("summary = %+v, want the mode change refused", summary)
	}
	if got := d.content(t, "a.txt"); string(got) != string(mine) {
		t.Fatalf("a.txt = %q, want the local edit on disk", got)
	}

	stored, err := d.store.GetFile(context.Background(), d.folder.ID, "a.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Hash != d.hasher.SumBytes(mine) {
		t.Fatalf("stored hash = %s, want the hash of the local edit", stored.Hash)
	}
	if !stored.Version.Equals(vector(t, "SOURCE:1,DEST:1")) {
		t.Fatalf("stored version = %s, want the one the scan wrote", stored.Version)
	}
}

// TestSync_RecordIsSkippedWhenTheStoredVersionMoved covers the second half of
// D1: whatever the cycle applies, it never overwrites a manifest row whose
// version moved since the plan was made.
func TestSync_RecordIsSkippedWhenTheStoredVersionMoved(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "gone.txt", []byte("what the peer deleted"))
	if err := src.store.MarkDeleted(ctx, src.folder.ID, []string{"gone.txt"}, sourceID, time.Now()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	mine := []byte("the version the user just wrote")
	d, conn := newRacingDest(t, src.server, func(d *dest) {
		d.record(t, "gone.txt", mine, vector(t, "SOURCE:1,DEST:1"))
	})
	d.record(t, "gone.txt", []byte("what the peer deleted"), vector(t, "SOURCE:1"))

	d.sync(t, conn)

	stored, err := d.store.GetFile(ctx, d.folder.ID, "gone.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Deleted {
		t.Fatal("the tombstone overwrote the row a local change had just written")
	}
	if !stored.Version.Equals(vector(t, "SOURCE:1,DEST:1")) {
		t.Fatalf("stored version = %s, want the one the scan wrote", stored.Version)
	}
	if got := d.events.Count("sync.record_superseded"); got != 1 {
		t.Fatalf("sync.record_superseded events = %d, want 1", got)
	}
}

// TestSync_AReceivedManifestRaisesTheCounterFloor is what makes a restored
// database safe: the peer holds counters this device once wrote, so the first
// contact lifts the floor back above them and the next local change dominates
// what the peer has.
func TestSync_AReceivedManifestRaisesTheCounterFloor(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("what the peer holds"))
	// The peer's copy of the path carries six versions this device wrote
	// before it lost them.
	src.setVersion(t, "a.txt", destID, vector(t, string(destID)+":6"))

	d, conn := newDest(t, src.server)
	d.sync(t, conn)

	floor, err := d.store.CounterFloor(context.Background(), destID)
	if err != nil {
		t.Fatalf("counter floor: %v", err)
	}
	if floor != 6 {
		t.Fatalf("floor = %d, want the counter the peer holds for this device", floor)
	}
}
