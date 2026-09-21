package syncer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/conflict"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// ordered returns the two contents of a conflict in the order the keep-both
// rule puts them: the first keeps the path, the second is moved aside. Which
// of two byte slices that is depends on their hashes, so a test that wants a
// particular outcome asks here instead of guessing.
func ordered(h fakes.Hasher, a, b []byte) (keeps, loses []byte) {
	if conflict.KeepsPath(h.SumBytes(a), h.SumBytes(b)) {
		return a, b
	}
	return b, a
}

// conflictCopies returns the conflict copies of a path inside the destination.
func conflictCopies(d *dest, rel string) []string {
	var out []string
	prefix := d.folder.Path + "/"
	for _, p := range d.fs.Paths() {
		name := strings.TrimPrefix(p, prefix)
		if name != rel && conflict.IsCopy(name) {
			out = append(out, name)
		}
	}
	return out
}

// theConflict returns the single unresolved conflict of the destination.
func theConflict(t *testing.T, d *dest) ports.ConflictRecord {
	t.Helper()
	list, err := d.store.ListConflicts(context.Background(), d.folder.ID, false)
	if err != nil {
		t.Fatalf("list conflicts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d conflicts recorded, want 1: %+v", len(list), list)
	}
	return list[0]
}

func TestConflict_LocalVersionThatLosesIsKeptAsACopy(t *testing.T) {
	ctx := context.Background()
	hasher := fakes.NewHasher()
	base := []byte("what both sides had")
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, hasher.SumBytes(base))

	summary := d.sync(t, conn)
	if summary.Conflicts != 1 || summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want one conflict and no plain pull", summary)
	}

	// The peer's version takes the path, and the local one is beside it.
	if got := d.content(t, "a.txt"); string(got) != string(remote) {
		t.Fatalf("a.txt = %q, want the peer's version", got)
	}
	copies := conflictCopies(d, "a.txt")
	if len(copies) != 1 {
		t.Fatalf("conflict copies = %v, want exactly one", copies)
	}
	if got := d.content(t, copies[0]); string(got) != string(local) {
		t.Fatalf("%s = %q, want the local version", copies[0], got)
	}
	want := conflict.Name("a.txt", d.clock.Now(), peerID)
	if copies[0] != want {
		t.Fatalf("conflict copy = %q, want %q", copies[0], want)
	}

	// Both files are recorded as agreed with the peer, so the next local
	// scan sees no change to send back.
	for _, rel := range []string{"a.txt", copies[0]} {
		stored, err := d.store.GetFile(ctx, d.folder.ID, rel)
		if err != nil {
			t.Fatalf("stored entry for %s: %v", rel, err)
		}
		if stored.Hash != stored.SyncedHash || stored.Hash.IsZero() {
			t.Fatalf("%s stored as %+v, want its hash recorded as synced", rel, stored)
		}
	}

	record := theConflict(t, d)
	if record.Path != "a.txt" || record.ConflictPath != copies[0] {
		t.Fatalf("conflict = %+v, want it to name both files", record)
	}
	if record.LocalHash != hasher.SumBytes(local) || record.RemoteHash != hasher.SumBytes(remote) {
		t.Fatalf("conflict = %+v, want the two versions recorded", record)
	}
	if record.Reason != "both-modified" || record.Resolved || record.PeerID != peerID {
		t.Fatalf("conflict = %+v, want an open both-modified conflict with the peer", record)
	}
	if d.events.Count("sync.conflict") != 1 {
		t.Fatal("no conflict event was published")
	}
}

func TestConflict_LocalVersionThatKeepsThePathIsUntouched(t *testing.T) {
	hasher := fakes.NewHasher()
	base := []byte("what both sides had")
	local, remote := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, hasher.SumBytes(base))

	summary := d.sync(t, conn)
	if summary.Conflicts != 1 || summary.Pulled != 0 {
		t.Fatalf("summary = %+v, want one conflict and no pull", summary)
	}
	if got := d.content(t, "a.txt"); string(got) != string(local) {
		t.Fatalf("a.txt = %q, want the local version untouched", got)
	}
	if copies := conflictCopies(d, "a.txt"); len(copies) != 0 {
		t.Fatalf("conflict copies = %v, want none: the peer keeps both versions", copies)
	}
	// The peer writes the copy and sends it here as an ordinary file, so
	// there is nothing for the user to decide on this side.
	list, err := d.store.ListConflicts(context.Background(), d.folder.ID, true)
	if err != nil {
		t.Fatalf("list conflicts: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("conflicts = %+v, want none on the side that keeps the path", list)
	}
	if d.events.Count("sync.conflict") != 1 {
		t.Fatal("no conflict event was published")
	}
}

func TestConflict_CopyNameThatIsTakenGetsACounter(t *testing.T) {
	hasher := fakes.NewHasher()
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, hasher.SumBytes([]byte("what both sides had")))
	taken := conflict.Name("a.txt", d.clock.Now(), peerID)
	d.record(t, taken, []byte("an older conflict copy"), hasher.SumBytes([]byte("an older conflict copy")))

	d.sync(t, conn)

	if got := d.content(t, taken); string(got) != "an older conflict copy" {
		t.Fatalf("%s = %q, want the copy that was already there", taken, got)
	}
	counted := conflict.WithCounter(taken, 1)
	if got := d.content(t, counted); string(got) != string(local) {
		t.Fatalf("%s = %q, want the local version", counted, got)
	}
	if record := theConflict(t, d); record.ConflictPath != counted {
		t.Fatalf("conflict names %q, want %q", record.ConflictPath, counted)
	}
}

func TestConflict_PeerDeletedALocallyChangedFile(t *testing.T) {
	ctx := context.Background()
	hasher := fakes.NewHasher()
	agreed := []byte("what both sides had")

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", agreed)
	if err := src.store.MarkDeleted(ctx, src.folder.ID, []string{"a.txt"}, peerID, time.Now()); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	d, conn := newDest(t, src.server)
	local := []byte("edited here after the peer deleted it")
	d.record(t, "a.txt", local, hasher.SumBytes(agreed))

	summary := d.sync(t, conn)
	if summary.Conflicts != 1 || summary.Deleted != 0 {
		t.Fatalf("summary = %+v, want one conflict and no delete", summary)
	}
	if got := d.content(t, "a.txt"); string(got) != string(local) {
		t.Fatalf("a.txt = %q, want the local version kept", got)
	}

	record := theConflict(t, d)
	if record.Reason != "delete-vs-modify" || record.ConflictPath != "" {
		t.Fatalf("conflict = %+v, want a delete-vs-modify with no copy", record)
	}
	if record.LocalHash != hasher.SumBytes(local) || !record.RemoteHash.IsZero() {
		t.Fatalf("conflict = %+v, want the local version and no remote one", record)
	}

	// The same divergence reported again updates the row instead of
	// adding a second one.
	d.sync(t, conn)
	if again := theConflict(t, d); again.ID != record.ID {
		t.Fatalf("a second cycle recorded conflict %q, want %q", again.ID, record.ID)
	}
}

func TestConflict_PeerReplacedALocallyChangedFileByADirectory(t *testing.T) {
	hasher := fakes.NewHasher()
	src := newSource(t, folder.ModeSendReceive)
	src.addDir(t, "f")

	d, conn := newDest(t, src.server)
	local := []byte("the version the user is editing")
	d.record(t, "f", local, hasher.SumBytes([]byte("what both sides had")))

	summary := d.sync(t, conn)
	if summary.Conflicts != 1 || summary.Dirs != 0 {
		t.Fatalf("summary = %+v, want one conflict and no directory", summary)
	}
	if got := d.content(t, "f"); string(got) != string(local) {
		t.Fatalf("f = %q, want the local file untouched", got)
	}
	if copies := conflictCopies(d, "f"); len(copies) != 0 {
		t.Fatalf("conflict copies = %v, want none for a directory", copies)
	}

	record := theConflict(t, d)
	if record.Reason != "replace-vs-modify" || record.ConflictPath != "" {
		t.Fatalf("conflict = %+v, want a replace-vs-modify with no copy", record)
	}
	if record.LocalHash != hasher.SumBytes(local) || !record.RemoteHash.IsZero() {
		t.Fatalf("conflict = %+v, want the local version and a directory on the peer", record)
	}
}

func TestConflict_LocalFileChangedDuringTheDownloadIsNotMovedAside(t *testing.T) {
	hasher := fakes.NewHasher()
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, hasher.SumBytes([]byte("what both sides had")))
	// The file on disk moves on after the plan was made, which the stored
	// entry does not describe any more.
	edited := []byte("edited while the download ran, longer than before")
	d.fs.AddFile(d.folder.Path+"/a.txt", edited, 0o644)

	summary := d.sync(t, conn)
	if summary.Failed != 1 {
		t.Fatalf("summary = %+v, want the conflict to be reported as failed", summary)
	}
	if got := d.content(t, "a.txt"); string(got) != string(edited) {
		t.Fatalf("a.txt = %q, want the newest local version", got)
	}
	if copies := conflictCopies(d, "a.txt"); len(copies) != 0 {
		t.Fatalf("conflict copies = %v, want none", copies)
	}
}
