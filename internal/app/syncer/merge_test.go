package syncer_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/mergebase"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// The three contents of a merge: the version both devices had, and the two
// edits, which touch the first and the last line and so never meet. Which of
// the two edits is the local one is decided by their hashes, and the merged
// result is the same either way, so the tests do not care.
const (
	mergeBaseText = "one\ntwo\nthree\nfour\nfive\n"
	mergeEditHead = "ONE\ntwo\nthree\nfour\nfive\n"
	mergeEditTail = "one\ntwo\nthree\nfour\nFIVE\n"
	mergeWanted   = "ONE\ntwo\nthree\nfour\nFIVE\n"
)

// mergeCase is a both-modified conflict set up on both devices: the peer holds
// one edit, this device the other, and the version they came from is whatever
// the test planted as a merge base.
type mergeCase struct {
	dest *dest
	conn *fakes.PeerConn
	src  *source
}

// divergedText sets up a conflict on "notes.txt" between two edits of
// mergeBaseText, with the peer holding the version that keeps the path so the
// local side is the one that has to act. textMerge says whether the setting is
// on, base is the version recorded as the one both sides came from, and an
// empty baseVersion means none was ever recorded.
func divergedText(t *testing.T, editA, editB, base, baseVersion string, textMerge bool) mergeCase {
	t.Helper()
	ctx := context.Background()
	hasher := fakes.NewHasher()
	remote, local := ordered(hasher, []byte(editA), []byte(editB))

	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "notes.txt", remote)
	src.setVersion(t, "notes.txt", sourceID, vector(t, "SOURCE:2"))

	d, conn := newDest(t, src.server)
	d.record(t, "notes.txt", local, vector(t, "DEST:1,SOURCE:1"))
	if baseVersion != "" {
		content := []byte(base)
		err := d.store.SaveMergeBase(ctx, ports.MergeBase{
			FolderID: d.folder.ID, Path: "notes.txt", Version: vector(t, baseVersion),
			Hash: d.hasher.SumBytes(content), Content: content, SavedAt: d.clock.Now(),
		})
		if err != nil {
			t.Fatalf("save merge base: %v", err)
		}
	}
	if textMerge {
		d.puller.SetMergeBases(mergebase.New(d.store, d.fs, d.hasher, d.clock, nil))
	}
	return mergeCase{dest: d, conn: conn, src: src}
}

// keptBoth asserts that the conflict was settled the way it always was: the
// peer's version at the path, the local one beside it, and a conflict record
// for the user to decide about.
func keptBoth(t *testing.T, c mergeCase) {
	t.Helper()
	copies := conflictCopies(c.dest, "notes.txt")
	if len(copies) != 1 {
		t.Fatalf("conflict copies = %v, want exactly one: the merge had to fall back", copies)
	}
	record := theConflict(t, c.dest)
	if record.Reason != "both-modified" {
		t.Fatalf("conflict reason = %q, want both-modified", record.Reason)
	}
	if c.dest.events.Count("sync.merged") != 0 {
		t.Fatal("a merge was reported for a conflict that kept both versions")
	}
}

// noConflictRecorded asserts that nothing was left for the user to decide.
func noConflictRecorded(t *testing.T, c mergeCase) {
	t.Helper()
	list, err := c.dest.store.ListConflicts(context.Background(), c.dest.folder.ID, true)
	if err != nil {
		t.Fatalf("list conflicts: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("conflicts recorded = %+v, want none: the merge settled it", list)
	}
	if copies := conflictCopies(c.dest, "notes.txt"); len(copies) != 0 {
		t.Fatalf("conflict copies = %v, want none: the merge settled it", copies)
	}
}

func TestMerge_DisjointEditsAreMergedInPlace(t *testing.T) {
	c := divergedText(t, mergeEditHead, mergeEditTail, mergeBaseText, "SOURCE:1", true)
	c.dest.sync(t, c.conn)

	if got := string(c.dest.content(t, "notes.txt")); got != mergeWanted {
		t.Fatalf("notes.txt = %q, want %q", got, mergeWanted)
	}
	noConflictRecorded(t, c)
	if c.dest.events.Count("sync.merged") != 1 {
		t.Fatalf("sync.merged events = %d, want 1", c.dest.events.Count("sync.merged"))
	}
	if c.dest.events.Count("sync.conflict") != 0 {
		t.Fatal("a merged file was also reported as a conflict")
	}
}

// TestMerge_TheMergedVersionDominatesBothSides is what stops the peer from
// conflicting with the merge in turn: it has to read as newer than either of
// the two versions it was made from.
func TestMerge_TheMergedVersionDominatesBothSides(t *testing.T) {
	c := divergedText(t, mergeEditHead, mergeEditTail, mergeBaseText, "SOURCE:1", true)
	c.dest.sync(t, c.conn)

	got := c.dest.storedVersion(t, "notes.txt")
	for _, side := range []string{"DEST:1,SOURCE:1", "SOURCE:2"} {
		if got.Compare(vector(t, side)) != manifest.After {
			t.Fatalf("merged version %s does not dominate %s", got, side)
		}
	}
}

// TestMerge_SameRegionKeepsBoth is the fallback the feature is judged by: two
// edits of one line have no answer, so the conflict is the one the user
// resolves by hand.
func TestMerge_SameRegionKeepsBoth(t *testing.T) {
	c := divergedText(t, "one\nLOCAL\nthree\n", "one\nREMOTE\nthree\n",
		"one\ntwo\nthree\n", "SOURCE:1", true)
	c.dest.sync(t, c.conn)
	keptBoth(t, c)
}

// TestMerge_BinaryContentKeepsBoth: content with a NUL byte in it is never
// merged, whatever the setting says.
func TestMerge_BinaryContentKeepsBoth(t *testing.T) {
	c := divergedText(t, "one\n\x00local\nthree\n", "one\n\x00remote\nthree\nfour\n",
		"one\n\x00\nthree\n", "SOURCE:1", true)
	c.dest.sync(t, c.conn)
	keptBoth(t, c)
}

// TestMerge_WithoutABaseKeepsBoth covers the device that turned the setting on
// after the two versions had already diverged: it has no record of what they
// came from, so it does not guess.
func TestMerge_WithoutABaseKeepsBoth(t *testing.T) {
	c := divergedText(t, mergeEditHead, mergeEditTail, mergeBaseText, "", true)
	c.dest.sync(t, c.conn)
	keptBoth(t, c)
}

// TestMerge_ABaseNeitherSideDescendsFromIsNotUsed is the safety rule. A
// recorded version that is not an ancestor of both sides would make the merge
// read one side's own edit as the other side removing it, and the edit would
// be dropped. Such a candidate is refused and both versions are kept.
func TestMerge_ABaseNeitherSideDescendsFromIsNotUsed(t *testing.T) {
	// SOURCE:3 is newer than the peer's SOURCE:2, so neither side
	// descends from it.
	c := divergedText(t, mergeEditHead, mergeEditTail, mergeBaseText, "SOURCE:3", true)
	c.dest.sync(t, c.conn)
	keptBoth(t, c)
}

// TestMerge_SettingOffKeepsBoth proves the default: the same conflict, the
// same recorded base, and without the setting the outcome is the keep-both it
// has always been.
func TestMerge_SettingOffKeepsBoth(t *testing.T) {
	c := divergedText(t, mergeEditHead, mergeEditTail, mergeBaseText, "SOURCE:1", false)
	c.dest.sync(t, c.conn)
	keptBoth(t, c)
	if got := string(c.dest.content(t, "notes.txt")); got == mergeWanted {
		t.Fatal("the file was merged with the setting off")
	}
}

// TestMerge_ABaseThatDisagreesWithItsOwnHashIsNotUsed is the read-side half of
// the check the recorder makes when it writes. A row whose content no longer
// hashes to the hash stored beside it was changed after it was written, so it
// is not a version either side ever had; merging against it would drop
// whatever edit it appears to already hold. It is passed over and both
// versions are kept.
func TestMerge_ABaseThatDisagreesWithItsOwnHashIsNotUsed(t *testing.T) {
	c := divergedText(t, mergeEditHead, mergeEditTail, mergeBaseText, "", true)

	// The row a healthy recorder would have written, with its content
	// corrupted in place and its hash column left intact. The corruption
	// chosen is the one that does damage rather than the one that is
	// obvious: a base holding both edits already reads as each side having
	// reverted the other's, so an unchecked merge against it succeeds and
	// throws both edits away.
	corrupted := []byte(mergeWanted)
	err := c.dest.store.SaveMergeBase(context.Background(), ports.MergeBase{
		FolderID: c.dest.folder.ID, Path: "notes.txt", Version: vector(t, "SOURCE:1"),
		Hash: c.dest.hasher.SumBytes([]byte(mergeBaseText)), Content: corrupted,
		SavedAt: c.dest.clock.Now(),
	})
	if err != nil {
		t.Fatalf("save merge base: %v", err)
	}

	c.dest.sync(t, c.conn)
	keptBoth(t, c)
	if got := string(c.dest.content(t, "notes.txt")); got == mergeBaseText {
		t.Fatal("both edits were reverted by merging against a corrupted base")
	}
}
