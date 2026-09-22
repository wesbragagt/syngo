package syncer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/conflict"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
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

// copyNameOf returns the name the copy of the local version of rel gets: the
// name is a function of that version alone, its author and its modification
// time, so every device that moves it aside writes the same one.
func copyNameOf(t *testing.T, d *dest, rel string) string {
	t.Helper()
	e, err := d.store.GetFile(context.Background(), d.folder.ID, rel)
	if err != nil {
		t.Fatalf("get %s: %v", rel, err)
	}
	by := e.ModifiedBy
	if by == "" {
		by = destID
	}
	return conflict.Name(rel, e.ModTime, by)
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
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, vector(t, "DEST:1"))
	// The name the losing version will get, read while it is still the one
	// at the path.
	want := copyNameOf(t, d, "a.txt")

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
	if copies[0] != want {
		t.Fatalf("conflict copy = %q, want %q", copies[0], want)
	}

	// Both files are recorded with the version they carry, so the next
	// local scan sees no change to send back and the peer orders them
	// against what it holds.
	for rel, want := range map[string]manifest.Version{
		"a.txt":   vector(t, "DEST:1,SOURCE:1"),
		copies[0]: vector(t, "DEST:1"),
	} {
		stored, err := d.store.GetFile(ctx, d.folder.ID, rel)
		if err != nil {
			t.Fatalf("stored entry for %s: %v", rel, err)
		}
		if stored.Hash.IsZero() || !stored.Version.Equals(want) {
			t.Fatalf("%s stored as %+v, want version %q", rel, stored, want)
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
	local, remote := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, vector(t, "DEST:1"))

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

func TestConflict_CopyNameTakenByOtherContentGetsACounter(t *testing.T) {
	hasher := fakes.NewHasher()
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, vector(t, "DEST:1"))
	taken := copyNameOf(t, d, "a.txt")
	d.record(t, taken, []byte("an older conflict copy"), vector(t, "DEST:1"))

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

// TestConflict_CopyNameTakenByTheSameContentIsReused covers the copy another
// device already wrote: it carries the same name, because the name describes
// the version and not the meeting of two devices, and the same bytes. Writing
// a second copy beside it would leave the mesh with one copy per device pair.
func TestConflict_CopyNameTakenByTheSameContentIsReused(t *testing.T) {
	ctx := context.Background()
	hasher := fakes.NewHasher()
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, vector(t, "DEST:1"))
	taken := copyNameOf(t, d, "a.txt")
	d.record(t, taken, local, vector(t, "SOURCE:1"))

	d.sync(t, conn)

	if got := d.content(t, "a.txt"); string(got) != string(remote) {
		t.Fatalf("a.txt = %q, want the peer's version", got)
	}
	copies := conflictCopies(d, "a.txt")
	if len(copies) != 1 || copies[0] != taken {
		t.Fatalf("conflict copies = %v, want only the one already there", copies)
	}
	if got := d.content(t, taken); string(got) != string(local) {
		t.Fatalf("%s = %q, want the local version", taken, got)
	}
	if record := theConflict(t, d); record.ConflictPath != taken {
		t.Fatalf("conflict names %q, want %q", record.ConflictPath, taken)
	}
	// The reused copy is a version of its own path made here, so the peer
	// that has it under an older vector is not asked to conflict over it.
	stored, err := d.store.GetFile(ctx, d.folder.ID, taken)
	if err != nil {
		t.Fatalf("stored copy: %v", err)
	}
	if !stored.Version.Equals(vector(t, "DEST:1,SOURCE:1")) {
		t.Fatalf("copy version = %q, want the one it had raised here", stored.Version)
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
	d.record(t, "a.txt", local, vector(t, "DEST:1"))

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
	d.record(t, "f", local, vector(t, "DEST:1"))

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
	d.record(t, "a.txt", local, vector(t, "DEST:1"))
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

// TestConflict_FailedInstallPutsTheLocalVersionBack proves the keep-both path
// leaves nothing in the air: the local file is moved aside before the peer's
// version is installed, and an install that fails puts it back. Without that
// the path would be empty and unrecorded, and the next scan would send the
// peer a deletion of a file it just offered.
func TestConflict_FailedInstallPutsTheLocalVersionBack(t *testing.T) {
	ctx := context.Background()
	hasher := fakes.NewHasher()
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)

	d, conn := newDest(t, src.server)
	before := d.record(t, "a.txt", local, vector(t, "DEST:1"))
	d.fs.FailNextRenameTo(d.folder.Path+"/a.txt", errors.New("no space left on device"))

	if _, err := d.puller.SyncFolder(ctx, conn, d.folder); err == nil {
		t.Fatal("sync succeeded although the install could not be completed")
	}

	if got := d.content(t, "a.txt"); string(got) != string(local) {
		t.Fatalf("a.txt = %q, want the local version back in place", got)
	}
	if copies := conflictCopies(d, "a.txt"); len(copies) != 0 {
		t.Fatalf("conflict copies left behind: %v", copies)
	}
	stored, err := d.store.GetFile(ctx, d.folder.ID, "a.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if stored.Hash != before.Hash || stored.Deleted {
		t.Fatalf("stored = %+v, want the local version still recorded", stored)
	}

	// The next cycle, with the filesystem working again, settles it.
	if _, err := d.puller.SyncFolder(ctx, conn, d.folder); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := d.content(t, "a.txt"); string(got) != string(remote) {
		t.Fatalf("a.txt = %q, want the peer's version", got)
	}
	if copies := conflictCopies(d, "a.txt"); len(copies) != 1 {
		t.Fatalf("conflict copies = %v, want the local version kept beside it", copies)
	}
}

func TestConflict_KeepBothRecordsTheMergedVersionAndANewCopy(t *testing.T) {
	ctx := context.Background()
	hasher := fakes.NewHasher()
	remote, local := ordered(hasher, []byte("the peer's version"), []byte("the local version"))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", remote)
	src.setVersion(t, "a.txt", peerID, vector(t, "BASE:1,SOURCE:1"))

	d, conn := newDest(t, src.server)
	d.record(t, "a.txt", local, vector(t, "BASE:1,DEST:1"))
	if err := d.store.UpsertEntries(ctx, d.folder.ID, []manifest.Entry{localEntry(t, d, "a.txt", "DEST")}); err != nil {
		t.Fatalf("record the author: %v", err)
	}

	if summary := d.sync(t, conn); summary.Conflicts != 1 {
		t.Fatalf("summary = %+v, want one conflict", summary)
	}
	copies := conflictCopies(d, "a.txt")
	if len(copies) != 1 {
		t.Fatalf("conflict copies = %v, want exactly one", copies)
	}

	// The outcome of a conflict dominates both versions that went into it,
	// so a third device holding either one takes it without conflicting
	// again.
	path, err := d.store.GetFile(ctx, d.folder.ID, "a.txt")
	if err != nil {
		t.Fatalf("stored entry: %v", err)
	}
	if !path.Version.Equals(vector(t, "BASE:1,DEST:1,SOURCE:1")) {
		t.Fatalf("version of the path = %q, want the merge of both", path.Version)
	}
	if path.ModifiedBy != peerID {
		t.Fatalf("author of the path = %q, want the device that wrote the content", path.ModifiedBy)
	}

	// The copy is a version of its own path, made here, and it keeps the
	// author of the content it holds.
	copied, err := d.store.GetFile(ctx, d.folder.ID, copies[0])
	if err != nil {
		t.Fatalf("stored copy: %v", err)
	}
	if !copied.Version.Equals(vector(t, "DEST:1")) {
		t.Fatalf("version of the copy = %q, want this device's first version of it", copied.Version)
	}
	if copied.ModifiedBy != "DEST" {
		t.Fatalf("author of the copy = %q, want the author of the local version", copied.ModifiedBy)
	}
}

// localEntry returns a stored destination entry with its author set, which a
// scan would have recorded when the local version was written.
func localEntry(t *testing.T, d *dest, rel string, by device.ID) manifest.Entry {
	t.Helper()
	e, err := d.store.GetFile(context.Background(), d.folder.ID, rel)
	if err != nil {
		t.Fatalf("get %s: %v", rel, err)
	}
	e.ModifiedBy = by
	return e
}
