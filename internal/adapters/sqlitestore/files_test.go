package sqlitestore_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// hashOf builds a deterministic hash from a seed byte.
func hashOf(seed byte) manifest.Hash {
	var h manifest.Hash
	for i := range h {
		h[i] = seed
	}
	return h
}

func TestUpsertEntries_AssignsIncreasingSequence(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	first := []manifest.Entry{
		{Path: "a.txt", Kind: manifest.KindFile, Size: 1, Hash: hashOf(1), ModTime: testTime},
		{Path: "b.txt", Kind: manifest.KindFile, Size: 2, Hash: hashOf(2), ModTime: testTime},
	}
	if err := store.UpsertEntries(ctx, "docs", first); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{
		{Path: "a.txt", Kind: manifest.KindFile, Size: 9, Hash: hashOf(3), ModTime: testTime},
	}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	if m.Entries["a.txt"].Sequence != 3 || m.Entries["b.txt"].Sequence != 2 {
		t.Fatalf("sequences = a:%d b:%d, want a:3 b:2", m.Entries["a.txt"].Sequence, m.Entries["b.txt"].Sequence)
	}
	if m.Sequence != 3 {
		t.Fatalf("manifest sequence = %d, want 3", m.Sequence)
	}
	if m.Entries["a.txt"].Size != 9 {
		t.Fatalf("a.txt size = %d, want the updated value", m.Entries["a.txt"].Size)
	}
}

// TestUpsertEntriesIf_WritesOnlyTheRowsThatDidNotMove is the store half of the
// lost-edit fix: a cycle that planned against one version must not overwrite a
// row another writer has moved on since.
func TestUpsertEntriesIf_WritesOnlyTheRowsThatDidNotMove(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	planned := versionOf(t, "AAA:1")
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{
		{Path: "kept.txt", Kind: manifest.KindFile, Size: 1, Hash: hashOf(1), ModTime: testTime,
			Version: planned},
		{Path: "moved.txt", Kind: manifest.KindFile, Size: 2, Hash: hashOf(2), ModTime: testTime,
			Version: versionOf(t, "AAA:1,BBB:1")},
	}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	skipped, err := store.UpsertEntriesIf(ctx, "docs", []manifest.Entry{
		{Path: "kept.txt", Kind: manifest.KindFile, Size: 10, Hash: hashOf(3), ModTime: testTime,
			Version: versionOf(t, "AAA:2")},
		{Path: "moved.txt", Kind: manifest.KindFile, Size: 20, Hash: hashOf(4), ModTime: testTime,
			Version: versionOf(t, "AAA:2")},
		{Path: "new.txt", Kind: manifest.KindFile, Size: 30, Hash: hashOf(5), ModTime: testTime,
			Version: versionOf(t, "AAA:1")},
		{Path: "unchecked.txt", Kind: manifest.KindFile, Size: 40, Hash: hashOf(6), ModTime: testTime},
	}, map[string]manifest.Version{
		"kept.txt":  planned,
		"moved.txt": planned,
		// A path with no row at all is expected to carry the zero vector.
		"new.txt": {},
	})
	if err != nil {
		t.Fatalf("UpsertEntriesIf error: %v", err)
	}
	if !reflect.DeepEqual(skipped, []string{"moved.txt"}) {
		t.Fatalf("skipped = %v, want [moved.txt]", skipped)
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	if m.Entries["kept.txt"].Size != 10 {
		t.Fatalf("kept.txt = %+v, want the new version written", m.Entries["kept.txt"])
	}
	if m.Entries["moved.txt"].Size != 2 {
		t.Fatalf("moved.txt = %+v, want the stored version untouched", m.Entries["moved.txt"])
	}
	if m.Entries["new.txt"].Size != 30 || m.Entries["unchecked.txt"].Size != 40 {
		t.Fatalf("entries = %+v, want the unconditional rows written", m.Entries)
	}
}

// versionOf parses a version vector written as "ID:N,ID:N".
func versionOf(t *testing.T, s string) manifest.Version {
	t.Helper()
	v, err := manifest.ParseVersion(s)
	if err != nil {
		t.Fatalf("parse version %q: %v", s, err)
	}
	return v
}

func TestUpsertEntries_BatchesOver500(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	const count = 1201
	entries := make([]manifest.Entry, count)
	for i := range entries {
		entries[i] = manifest.Entry{
			Path: fmt.Sprintf("f%04d.txt", i), Kind: manifest.KindFile,
			Size: int64(i), Hash: hashOf(byte(i%255 + 1)), ModTime: testTime,
		}
	}
	if err := store.UpsertEntries(ctx, "docs", entries); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	if len(m.Entries) != count {
		t.Fatalf("stored %d entries, want %d", len(m.Entries), count)
	}
	if m.Sequence != count {
		t.Fatalf("manifest sequence = %d, want %d", m.Sequence, count)
	}
	seen := make(map[uint64]bool, count)
	for _, e := range m.Entries {
		if seen[e.Sequence] {
			t.Fatalf("duplicate sequence %d", e.Sequence)
		}
		seen[e.Sequence] = true
	}
}

func TestLoadManifest_RoundTripsAllFields(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	mtime := time.Unix(1700000000, 123456789).UTC()
	want := []manifest.Entry{
		{
			Path: "dir/a.txt", Kind: manifest.KindFile, Size: 42, ModTime: mtime, Mode: 0o640,
			Hash: hashOf(7), Inode: 99, ModifiedBy: "DEVICEID", SyncedMode: 0o600,
		},
		{Path: "dir", Kind: manifest.KindDir, ModTime: mtime, Mode: 0o755},
		{Path: "link", Kind: manifest.KindSymlink, ModTime: mtime, LinkTarget: "dir/a.txt"},
		{Path: "gone.txt", Kind: manifest.KindFile, ModTime: mtime, Deleted: true},
	}
	if err := store.UpsertEntries(ctx, "docs", want); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	for _, w := range want {
		got := m.Entries[w.Path]
		w.Sequence = got.Sequence
		if !got.ModTime.Equal(w.ModTime) {
			t.Fatalf("%s mtime = %v, want %v", w.Path, got.ModTime, w.ModTime)
		}
		got.ModTime = w.ModTime
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("%s = %+v, want %+v", w.Path, got, w)
		}
	}
	if len(m.Live()) != 3 {
		t.Fatalf("live entries = %d, want 3", len(m.Live()))
	}

	if err := store.SetSyncedMode(ctx, "docs", "dir/a.txt", 0o700); err != nil {
		t.Fatalf("SetSyncedMode error: %v", err)
	}
	m, err = store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	if got := m.Entries["dir/a.txt"].SyncedMode; got != 0o700 {
		t.Fatalf("synced mode = %o, want 700", got)
	}
	if err := store.SetSyncedMode(ctx, "docs", "missing.txt", 0o700); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("SetSyncedMode on unknown path = %v, want ErrNotFound", err)
	}
}

func TestMarkDeleted_CreatesTombstone(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{
		{Path: "a.txt", Kind: manifest.KindFile, Size: 5, Hash: hashOf(1), ModTime: testTime},
	}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	deletedAt := testTime.Add(time.Hour)
	if err := store.MarkDeleted(ctx, "docs", []string{"a.txt", "unknown.txt"}, "DEVICEID", deletedAt); err != nil {
		t.Fatalf("MarkDeleted error: %v", err)
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	got := m.Entries["a.txt"]
	if !got.Deleted || !got.Hash.IsZero() || got.Size != 0 || got.ModifiedBy != "DEVICEID" ||
		!got.ModTime.Equal(deletedAt) || got.Sequence != 2 {
		t.Fatalf("tombstone = %+v", got)
	}
	// The deletion is a content change of the deleting device, so the
	// tombstone dominates the version it replaced.
	if got.Version.Counter("DEVICEID") != 1 {
		t.Fatalf("tombstone version = %q, want the counter of DEVICEID at 1", got.Version)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("entries = %d, want only the tombstone", len(m.Entries))
	}
}

func TestFolderStats(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{
		{Path: "a.txt", Kind: manifest.KindFile, Size: 10, Hash: hashOf(1), ModTime: testTime},
		{Path: "b.txt", Kind: manifest.KindFile, Size: 32, Hash: hashOf(2), ModTime: testTime},
		{Path: "dir", Kind: manifest.KindDir, ModTime: testTime},
		{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "a.txt", ModTime: testTime},
		{Path: "gone.txt", Kind: manifest.KindFile, Size: 99, Deleted: true, ModTime: testTime},
	}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	stats, err := store.FolderStats(ctx, "docs")
	if err != nil {
		t.Fatalf("FolderStats error: %v", err)
	}
	want := ports.FolderStats{Files: 2, Dirs: 1, Symlinks: 1, Bytes: 42}
	if stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}

	empty, err := store.FolderStats(ctx, "unknown")
	if err != nil {
		t.Fatalf("FolderStats on unknown folder error: %v", err)
	}
	if (empty != ports.FolderStats{}) {
		t.Fatalf("stats of unknown folder = %+v, want zero", empty)
	}
}

func TestAppendAndRecentChanges(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	newEntry := manifest.Entry{Path: "b.txt", Kind: manifest.KindFile, Hash: hashOf(2)}
	oldEntry := manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Hash: hashOf(1)}
	changes := []manifest.Change{
		{Type: manifest.Create, Path: "b.txt", New: &newEntry},
		{Type: manifest.Rename, Path: "b.txt", OldPath: "a.txt", Old: &oldEntry, New: &newEntry},
	}
	if err := store.AppendChanges(ctx, "docs", changes, testTime); err != nil {
		t.Fatalf("AppendChanges error: %v", err)
	}

	recs, err := store.RecentChanges(ctx, "docs", 1)
	if err != nil {
		t.Fatalf("RecentChanges error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Type != manifest.Rename || rec.Path != "b.txt" || rec.OldPath != "a.txt" ||
		rec.OldHash != hashOf(1) || rec.NewHash != hashOf(2) || !rec.At.Equal(testTime) || rec.FolderID != "docs" {
		t.Fatalf("record = %+v", rec)
	}

	all, err := store.RecentChanges(ctx, "docs", 0)
	if err != nil {
		t.Fatalf("RecentChanges error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("records = %d, want 2", len(all))
	}
}

func TestApplyScan_RejectsAStaleWrite(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}

	entry := manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Hash: hashOf(1)}
	write := ports.ScanWrite{
		BaseSequence: 0,
		Upserts:      []manifest.Entry{entry},
		Changes:      []manifest.Change{{Type: manifest.Create, Path: "a.txt", New: &entry}},
		By:           "DEVICEID",
		At:           testTime,
	}
	if err := store.ApplyScan(ctx, "docs", write); err != nil {
		t.Fatalf("first ApplyScan error: %v", err)
	}

	// A second scan that started from the same manifest must not append its
	// changes a second time.
	err := store.ApplyScan(ctx, "docs", write)
	if !errors.Is(err, ports.ErrStaleManifest) {
		t.Fatalf("stale ApplyScan error = %v, want ErrStaleManifest", err)
	}

	recs, err := store.RecentChanges(ctx, "docs", 0)
	if err != nil {
		t.Fatalf("RecentChanges error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("change rows = %d, want 1", len(recs))
	}

	m, err := store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("LoadManifest error: %v", err)
	}
	write.BaseSequence = m.Sequence
	write.LastError = "1 entry/entries skipped: locked: permission denied"
	if err := store.ApplyScan(ctx, "docs", write); err != nil {
		t.Fatalf("ApplyScan from the current sequence error: %v", err)
	}

	f, err := store.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("GetFolder error: %v", err)
	}
	if f.LastError != write.LastError || !f.LastScanAt.Equal(testTime) {
		t.Fatalf("folder scan state = %q at %v", f.LastError, f.LastScanAt)
	}
}

func TestVersion_RoundTripsAndIsBumpedByDeletions(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	if err := store.SaveFolder(ctx, testFolder("docs", "/home/u/docs")); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	stored, err := manifest.ParseVersion("AAA:2,BBB:1")
	if err != nil {
		t.Fatalf("ParseVersion error: %v", err)
	}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{
		{Path: "a.txt", Kind: manifest.KindFile, Size: 5, Hash: hashOf(1), ModTime: testTime, Version: stored},
	}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}

	got, err := store.GetFile(ctx, "docs", "a.txt")
	if err != nil {
		t.Fatalf("GetFile error: %v", err)
	}
	if !got.Version.Equals(stored) {
		t.Fatalf("version = %q, want %q", got.Version, stored)
	}

	// SetVersion replaces the vector without giving the row a new sequence:
	// the content is unchanged, only its history is now better known.
	merged := stored.Bump("CCC")
	if err := store.SetVersion(ctx, "docs", "a.txt", merged); err != nil {
		t.Fatalf("SetVersion error: %v", err)
	}
	after, err := store.GetFile(ctx, "docs", "a.txt")
	if err != nil {
		t.Fatalf("GetFile error: %v", err)
	}
	if !after.Version.Equals(merged) || after.Sequence != got.Sequence {
		t.Fatalf("after SetVersion = %q at sequence %d, want %q at %d",
			after.Version, after.Sequence, merged, got.Sequence)
	}
	if err := store.SetVersion(ctx, "docs", "missing.txt", merged); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("SetVersion on unknown path = %v, want ErrNotFound", err)
	}

	// A deletion carries the vector it replaces, raised by one.
	if err := store.MarkDeleted(ctx, "docs", []string{"a.txt"}, "BBB", testTime); err != nil {
		t.Fatalf("MarkDeleted error: %v", err)
	}
	tombstone, err := store.GetFile(ctx, "docs", "a.txt")
	if err != nil {
		t.Fatalf("GetFile error: %v", err)
	}
	if want := merged.Bump("BBB"); !tombstone.Version.Equals(want) {
		t.Fatalf("tombstone version = %q, want %q", tombstone.Version, want)
	}
}
