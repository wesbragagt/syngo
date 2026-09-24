package syncer_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/counterfloor"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/syncplan"
)

// markProvisional turns a stored entry into one this device wrote from a
// counter floor no peer had confirmed, which is what the scanner records
// between a restore from a backup and the first contact.
func markProvisional(t *testing.T, d *dest, rel string) {
	t.Helper()
	ctx := context.Background()
	e, err := d.store.GetFile(ctx, d.folder.ID, rel)
	if err != nil {
		t.Fatalf("get %s: %v", rel, err)
	}
	e.Provisional = true
	e.ModifiedBy = destID
	if err := d.store.UpsertEntries(ctx, d.folder.ID, []manifest.Entry{e}); err != nil {
		t.Fatalf("mark %s provisional: %v", rel, err)
	}
}

// TestSync_AnEditFromAnUnconfirmedFloorIsNotOverwritten is the restore window.
// The database was put back from a backup recording DEST:1, the user edited
// the file before the daemon reached anyone, and the edit was given DEST:2.
// The peer holds DEST:3, a version this device wrote before the restore and
// lost. By the vectors alone the peer's version descends from the edit and
// replaces it; in fact the two were written independently.
//
// Both are kept. A conflict the user resolves by hand beats an automatic
// resolution that throws the edit away.
func TestSync_AnEditFromAnUnconfirmedFloorIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("the version the peer kept"))
	src.setVersion(t, "a.txt", destID, vector(t, string(destID)+":3"))

	d, conn := newDest(t, src.server)
	d.puller.SetCounterFloorTrust(counterfloor.New())
	mine := []byte("the edit made after the restore")
	d.record(t, "a.txt", mine, vector(t, string(destID)+":2"))
	markProvisional(t, d, "a.txt")
	want := copyNameOf(t, d, "a.txt")

	summary := d.sync(t, conn)
	if summary.Conflicts != 1 {
		t.Fatalf("summary = %+v, want the edit reported as a conflict", summary)
	}

	// The edit is still on disk, under the name the losing version gets.
	copies := conflictCopies(d, "a.txt")
	if len(copies) != 1 || copies[0] != want {
		t.Fatalf("conflict copies = %v, want exactly [%s] holding the edit", copies, want)
	}
	if got := string(d.content(t, want)); got != string(mine) {
		t.Fatalf("%s holds %q, want the edit made after the restore", want, got)
	}
	// The peer's version took the path, so the two devices converge on it
	// and the copy travels to the peer as an ordinary file.
	if got := string(d.content(t, "a.txt")); got != "the version the peer kept" {
		t.Fatalf("a.txt holds %q, want the peer's version", got)
	}

	rec := theConflict(t, d)
	if rec.Reason != syncplan.ReasonUnverifiedFloor || rec.ConflictPath != want {
		t.Fatalf("conflict = %+v, want reason %q at %s",
			rec, syncplan.ReasonUnverifiedFloor, want)
	}

	// The manifest describes both files, so nothing on disk is unrecorded.
	m, err := d.store.LoadManifest(ctx, d.folder.ID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if m.Entries[want].Hash != d.hasher.SumBytes(mine) {
		t.Fatalf("the copy is recorded as %+v, want the edit's hash", m.Entries[want])
	}
	if m.Entries["a.txt"].Provisional {
		t.Fatal("a.txt is still provisional after the conflict settled it")
	}
}

// TestSync_TheFirstManifestConfirmsTheFloor is the other half: once a peer has
// been heard from, the floor is above every counter of this device it holds,
// and the scanner stops marking what it writes.
func TestSync_TheFirstManifestConfirmsTheFloor(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("what the peer holds"))
	src.setVersion(t, "a.txt", destID, vector(t, string(destID)+":6"))

	d, conn := newDest(t, src.server)
	trust := counterfloor.New()
	d.puller.SetCounterFloorTrust(trust)
	if trust.Verified() {
		t.Fatal("the floor is confirmed before any peer was heard from")
	}

	d.sync(t, conn)

	if !trust.Verified() {
		t.Fatal("the floor is still unconfirmed after a peer sent its manifest")
	}
}

// TestSync_AManifestWithoutOurCountersConfirmsTheFloorToo covers the peer that
// holds no counter of this device at all: it holds nothing that could dominate
// this device's counters, so there is nothing left to doubt.
func TestSync_AManifestWithoutOurCountersConfirmsTheFloorToo(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", []byte("what the peer holds"))
	src.setVersion(t, "a.txt", sourceID, vector(t, "SOURCE:1"))

	d, conn := newDest(t, src.server)
	trust := counterfloor.New()
	d.puller.SetCounterFloorTrust(trust)

	d.sync(t, conn)

	if !trust.Verified() {
		t.Fatal("a manifest naming no counter of this device left the floor unconfirmed")
	}
}

// TestSync_AnAgreementClearsTheProvisionalMark is what keeps the doubt from
// outliving its cause: a peer found holding the same content accepts the
// counter the version carries, so the version is no longer in question.
func TestSync_AnAgreementClearsTheProvisionalMark(t *testing.T) {
	ctx := context.Background()
	same := []byte("the same bytes on both sides")

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "a.txt", same)
	src.setVersion(t, "a.txt", destID, vector(t, string(destID)+":3"))

	d, conn := newDest(t, src.server)
	d.puller.SetCounterFloorTrust(counterfloor.New())
	d.record(t, "a.txt", same, vector(t, string(destID)+":2"))
	markProvisional(t, d, "a.txt")

	d.sync(t, conn)

	got, err := d.store.GetFile(ctx, d.folder.ID, "a.txt")
	if err != nil {
		t.Fatalf("get a.txt: %v", err)
	}
	if got.Provisional {
		t.Fatal("a.txt is still provisional after a peer was found holding the same content")
	}
	if got.Version.Counter(destID) != 3 {
		t.Fatalf("version = %q, want the two vectors merged", got.Version)
	}
}
