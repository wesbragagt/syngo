package syncplan_test

import (
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/syncplan"
)

// hash returns a distinct hash for a label.
func hash(label string) manifest.Hash {
	var h manifest.Hash
	copy(h[:], label)
	if label == "" {
		return manifest.Hash{}
	}
	return h
}

// testFolder returns a receiving folder with the default policies.
func testFolder() folder.Folder {
	return folder.Folder{
		ID: "docs", Path: "/srv/docs", Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkStore, KeepEmptyDirs: true,
	}
}

// vec builds a version vector from the stored form, "ID:N,ID:N". The input is
// written by the test itself, so anything it cannot read is a broken test.
func vec(s string) manifest.Version {
	v, err := manifest.ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// file returns a live file entry carrying a version vector.
func file(path, content string, v manifest.Version) manifest.Entry {
	return manifest.Entry{Path: path, Kind: manifest.KindFile, Size: 1, Hash: hash(content), Version: v}
}

// modedFile returns a live file entry carrying permission bits and the mode
// both sides last agreed on.
func modedFile(path, content string, v manifest.Version, mode, syncedMode uint32) manifest.Entry {
	e := file(path, content, v)
	e.Mode = mode
	e.SyncedMode = syncedMode
	return e
}

// tombstone returns a deleted entry carrying a version vector.
func tombstone(path string, v manifest.Version) manifest.Entry {
	return manifest.Entry{Path: path, Kind: manifest.KindFile, Deleted: true, Version: v}
}

// dirAt returns a directory entry carrying a version vector.
func dirAt(path string, v manifest.Version) manifest.Entry {
	return manifest.Entry{Path: path, Kind: manifest.KindDir, Mode: 0o755, Version: v}
}

// manifestOf builds a manifest from entries.
func manifestOf(entries ...manifest.Entry) manifest.Manifest {
	m := manifest.New("docs")
	for _, e := range entries {
		m.Add(e)
	}
	return m
}

// only returns the single item of a plan, failing when there is not exactly one.
func only(t *testing.T, items []syncplan.Item) syncplan.Item {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("plan has %d items, want 1: %+v", len(items), items)
	}
	return items[0]
}

func TestPlan_Rules(t *testing.T) {
	link := manifest.Entry{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "target"}

	tests := []struct {
		name   string
		local  manifest.Manifest
		remote manifest.Manifest
		folder folder.Folder
		action syncplan.Action
		reason string
	}{
		{
			name:   "missing locally is pulled",
			local:  manifestOf(),
			remote: manifestOf(file("a.txt", "v2", vec("A:2"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonMissing,
		},
		{
			name: "a local tombstone the peer's version outlives is pulled again",
			// The peer wrote on top of the version this side deleted,
			// so its content is the surviving one.
			local:  manifestOf(tombstone("a.txt", vec("A:1"))),
			remote: manifestOf(file("a.txt", "v2", vec("A:2"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonMissing,
		},
		{
			name: "a local tombstone concurrent with the peer's version keeps the content",
			// Neither side saw the other; a file that comes back can
			// be deleted again, one that is gone cannot come back.
			local:  manifestOf(tombstone("a.txt", vec("B:1"))),
			remote: manifestOf(file("a.txt", "v2", vec("A:2"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonMissing,
		},
		{
			name: "a local tombstone that dominates a stale live version stays deleted",
			// KF-3: the peer was away while the file was deleted here.
			local:  manifestOf(tombstone("a.txt", vec("A:2"))),
			remote: manifestOf(file("a.txt", "v1", vec("A:1"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "same hash is in sync",
			local:  manifestOf(file("a.txt", "v1", vec("A:1"))),
			remote: manifestOf(file("a.txt", "v1", vec("A:1"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name: "same hash under concurrent vectors is in sync",
			// The same content reached both sides by different routes;
			// the puller merges the two vectors and nothing moves.
			local:  manifestOf(file("a.txt", "v1", vec("A:1,B:1"))),
			remote: manifestOf(file("a.txt", "v1", vec("A:1,C:1"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "a version made on top of ours is pulled",
			local:  manifestOf(file("a.txt", "v1", vec("A:1"))),
			remote: manifestOf(file("a.txt", "v2", vec("A:2"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonRemoteNewer,
		},
		{
			name: "an earlier version of our own change is not a conflict",
			// KF-3: the peer holds a version this device wrote and has
			// not caught up with the newer one, which it pulls itself.
			local:  manifestOf(file("a.txt", "v3", vec("A:3"))),
			remote: manifestOf(file("a.txt", "v2", vec("A:2"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "two devices changing the same file is a conflict",
			local:  manifestOf(file("a.txt", "local", vec("A:1,B:1"))),
			remote: manifestOf(file("a.txt", "remote", vec("A:2"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonBothModified,
		},
		{
			name: "the outcome of a conflict still conflicts with a third version",
			// KF-2: the local entry is what a conflict on this device
			// installed, so it dominates two of the three versions but
			// not the one the third device made on its own.
			local:  manifestOf(file("a.txt", "vA", vec("A:2,B:1"))),
			remote: manifestOf(file("a.txt", "vC", vec("A:1,C:1"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonBothModified,
		},
		{
			name: "the outcome of a conflict is taken by a device holding one of its inputs",
			// KF-2 compositionality: the outcome dominates the input,
			// so the third device takes it without conflicting again.
			local:  manifestOf(file("a.txt", "vB", vec("A:1,B:1"))),
			remote: manifestOf(file("a.txt", "vA", vec("A:2,B:1"))),
			action: syncplan.ActionPull, reason: syncplan.ReasonRemoteNewer,
		},
		{
			name: "two versions without vectors and different content is a conflict",
			// Rows that predate version vectors: the empty vector says
			// nothing about which version came first, so both are kept.
			local:  manifestOf(file("a.txt", "local", manifest.Version{})),
			remote: manifestOf(file("a.txt", "remote", manifest.Version{})),
			action: syncplan.ActionConflict, reason: syncplan.ReasonBothModified,
		},
		{
			name:   "two versions without vectors and the same content are in sync",
			local:  manifestOf(file("a.txt", "v1", manifest.Version{})),
			remote: manifestOf(file("a.txt", "v1", manifest.Version{})),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "remote deletion of a dominated file is applied",
			local:  manifestOf(file("a.txt", "v1", vec("A:1"))),
			remote: manifestOf(tombstone("a.txt", vec("A:2"))),
			action: syncplan.ActionDelete, reason: syncplan.ReasonRemoteDeleted,
		},
		{
			name:   "remote deletion against a concurrent edit is a conflict",
			local:  manifestOf(file("a.txt", "v2", vec("A:1,B:1"))),
			remote: manifestOf(tombstone("a.txt", vec("A:2"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonDeleteVsModify,
		},
		{
			name: "a stale deletion does not remove a newer file",
			// KF-3 the other way round: the peer deleted a version this
			// side has already written past.
			local:  manifestOf(file("a.txt", "v3", vec("A:3"))),
			remote: manifestOf(tombstone("a.txt", vec("A:2"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "remote deletion of a directory is applied",
			local:  manifestOf(dirAt("sub", vec("A:1"))),
			remote: manifestOf(manifest.Entry{Path: "sub", Kind: manifest.KindDir, Deleted: true}),
			action: syncplan.ActionDelete, reason: syncplan.ReasonRemoteDeleted,
		},
		{
			name:   "missing directory is created",
			local:  manifestOf(),
			remote: manifestOf(dirAt("sub", vec("A:1"))),
			action: syncplan.ActionMkDir, reason: syncplan.ReasonMissing,
		},
		{
			name:   "existing directory is in sync",
			local:  manifestOf(dirAt("sub", vec("A:1"))),
			remote: manifestOf(dirAt("sub", vec("A:1"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "a file the peer turned into a directory is replaced",
			local:  manifestOf(file("f", "v1", vec("A:1"))),
			remote: manifestOf(dirAt("f", vec("A:2"))),
			action: syncplan.ActionReplace, reason: syncplan.ReasonKindChanged,
		},
		{
			name:   "a concurrently modified file the peer turned into a directory is a conflict",
			local:  manifestOf(file("f", "local", vec("A:1,B:1"))),
			remote: manifestOf(dirAt("f", vec("A:2"))),
			action: syncplan.ActionConflict, reason: syncplan.ReasonReplaceVsModify,
		},
		{
			name:   "a file that dominates the peer's directory is kept",
			local:  manifestOf(file("f", "v3", vec("A:3"))),
			remote: manifestOf(dirAt("f", vec("A:2"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name: "a kind change without vectors replaces the file",
			// Two empty vectors compare Equal, which is the backfill
			// case: the peer's directory is taken and the file it
			// replaces is recoverable from the peer that still has it.
			local:  manifestOf(file("f", "local", manifest.Version{})),
			remote: manifestOf(dirAt("f", manifest.Version{})),
			action: syncplan.ActionReplace, reason: syncplan.ReasonKindChanged,
		},
		{
			name:   "a directory the peer turned into a file is replaced",
			local:  manifestOf(dirAt("sub", vec("A:1"))),
			remote: manifestOf(file("sub", "v1", vec("A:2"))),
			action: syncplan.ActionReplace, reason: syncplan.ReasonKindChanged,
		},
		{
			name:   "a symlink the peer turned into a file is replaced",
			local:  manifestOf(manifest.Entry{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "target"}),
			remote: manifestOf(file("link", "v1", vec("A:2"))),
			action: syncplan.ActionReplace, reason: syncplan.ReasonKindChanged,
		},
		{
			name:   "a mode the peer changed is applied",
			local:  manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o644, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o700, 0)),
			action: syncplan.ActionChmod, reason: syncplan.ReasonModeChanged,
		},
		{
			name:   "a mode changed locally is left for the peer to take",
			local:  manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o700, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o644, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "a mode without a baseline is not applied",
			local:  manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o644, 0)),
			remote: manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o700, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonModeUnknown,
		},
		{
			name: "a mode is not applied to a locally modified file",
			// The local content change is concurrent with the peer's
			// mode change, so the content rule answers first.
			local:  manifestOf(modedFile("a.txt", "v2", vec("A:1,B:1"), 0o644, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o700, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "equal modes are in sync",
			local:  manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o644, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o644, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "a peer that announces no mode changes nothing",
			local:  manifestOf(modedFile("a.txt", "v1", vec("A:1"), 0o644, 0o644)),
			remote: manifestOf(file("a.txt", "v1", vec("A:1"))),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "symlink is created when the policy stores them",
			local:  manifestOf(),
			remote: manifestOf(link),
			action: syncplan.ActionSymlink, reason: syncplan.ReasonMissing,
		},
		{
			name:   "symlink with a different target is recreated",
			local:  manifestOf(manifest.Entry{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "other"}),
			remote: manifestOf(link),
			action: syncplan.ActionSymlink, reason: syncplan.ReasonMissing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.folder
			if f.ID == "" {
				f = testFolder()
			}
			item := only(t, syncplan.Plan(tc.local, tc.remote, f))
			if item.Action != tc.action || item.Reason != tc.reason {
				t.Fatalf("got %s(%s), want %s(%s)", item.Action, item.Reason, tc.action, tc.reason)
			}
		})
	}
}

func TestPlan_SymlinkPolicySkipDoesNotCreate(t *testing.T) {
	f := testFolder()
	f.Symlinks = folder.SymlinkSkip
	remote := manifestOf(manifest.Entry{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "target"})

	item := only(t, syncplan.Plan(manifestOf(), remote, f))
	if item.Action != syncplan.ActionNone || item.Reason != syncplan.ReasonSymlinkPolicy {
		t.Fatalf("got %s(%s), want none(%s)", item.Action, item.Reason, syncplan.ReasonSymlinkPolicy)
	}
}

func TestPlan_DeletePolicyNeverKeepsTheFile(t *testing.T) {
	f := testFolder()
	f.DeletePolicy = folder.DeleteNever

	local := manifestOf(file("a.txt", "v1", vec("A:1")))
	item := only(t, syncplan.Plan(local, manifestOf(tombstone("a.txt", vec("A:2"))), f))
	if item.Action != syncplan.ActionNone || item.Reason != syncplan.ReasonDeletePolicyNever {
		t.Fatalf("got %s(%s), want none(%s)", item.Action, item.Reason, syncplan.ReasonDeletePolicyNever)
	}
}

func TestPlan_SendOnlyProducesNothing(t *testing.T) {
	f := testFolder()
	f.Mode = folder.ModeSendOnly

	if items := syncplan.Plan(manifestOf(), manifestOf(file("a.txt", "v1", vec("A:1"))), f); len(items) != 0 {
		t.Fatalf("send-only folder planned %+v, want nothing", items)
	}
}

func TestPlan_RemoteTombstoneWithoutLocalEntryIsIgnored(t *testing.T) {
	remote := manifestOf(tombstone("a.txt", vec("A:2")))
	if items := syncplan.Plan(manifestOf(), remote, testFolder()); len(items) != 0 {
		t.Fatalf("planned %+v for a path neither side has, want nothing", items)
	}
	local := manifestOf(tombstone("a.txt", vec("A:2")))
	if items := syncplan.Plan(local, remote, testFolder()); len(items) != 0 {
		t.Fatalf("planned %+v for two tombstones, want nothing", items)
	}
}

func TestPlan_DirsBeforeFilesAndDeletesLast(t *testing.T) {
	local := manifestOf(file("old/gone.txt", "v1", vec("A:1")), dirAt("old", vec("A:1")))
	remote := manifestOf(
		file("b/deep.txt", "v1", vec("A:1")),
		file("a.txt", "v1", vec("A:1")),
		dirAt("b", vec("A:1")),
		tombstone("old/gone.txt", vec("A:2")),
		manifest.Entry{Path: "old", Kind: manifest.KindDir, Deleted: true, Version: vec("A:2")},
	)

	var order []string
	for _, item := range syncplan.Plan(local, remote, testFolder()) {
		order = append(order, item.Action.String()+":"+item.Path)
	}
	want := []string{"mkdir:b", "pull:a.txt", "pull:b/deep.txt", "delete:old/gone.txt", "delete:old"}
	if len(order) != len(want) {
		t.Fatalf("plan = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("plan = %v, want %v", order, want)
		}
	}
}

func TestPlan_DirReplacedByAFileComesAfterItsChildrenAreDeleted(t *testing.T) {
	local := manifestOf(
		dirAt("sub", vec("A:1")),
		file("sub/inner.txt", "v1", vec("A:1")),
	)
	remote := manifestOf(
		file("sub", "v2", vec("A:2")),
		tombstone("sub/inner.txt", vec("A:2")),
	)

	var order []string
	for _, item := range syncplan.Plan(local, remote, testFolder()) {
		order = append(order, item.Action.String()+":"+item.Path)
	}
	want := []string{"delete:sub/inner.txt", "replace:sub"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("plan = %v, want %v", order, want)
	}
}

func TestPlan_FileReplacedByADirComesBeforeItsChildren(t *testing.T) {
	local := manifestOf(file("f", "v1", vec("A:1")))
	remote := manifestOf(
		file("f/inner.txt", "v2", vec("A:2")),
		dirAt("f", vec("A:2")),
	)

	var order []string
	for _, item := range syncplan.Plan(local, remote, testFolder()) {
		order = append(order, item.Action.String()+":"+item.Path)
	}
	want := []string{"replace:f", "pull:f/inner.txt"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("plan = %v, want %v", order, want)
	}
}

// TestPlan_ChildrenOfAConflictedFileAreNotPulled covers the second half of a
// replace-vs-modify conflict: the local file keeps the path, so nothing can be
// written beneath it until the conflict is settled. Planning the children as
// pulls would make the puller try, fail on the parent and count a failure for
// every one of them.
func TestPlan_ChildrenOfAConflictedFileAreNotPulled(t *testing.T) {
	local := manifestOf(file("b.txt", "local", vec("A:1,B:1")))
	remote := manifestOf(
		dirAt("b.txt", vec("A:2")),
		file("b.txt/x.txt", "inner", vec("A:2")),
		dirAt("b.txt/deep", vec("A:2")),
		file("b.txt/deep/y.txt", "deeper", vec("A:2")),
		file("unrelated.txt", "elsewhere", vec("A:2")),
	)

	got := make(map[string]string)
	for _, item := range syncplan.Plan(local, remote, testFolder()) {
		got[item.Path] = item.Action.String() + ":" + item.Reason
	}
	want := map[string]string{
		"b.txt":            "conflict:" + syncplan.ReasonReplaceVsModify,
		"b.txt/x.txt":      "none:" + syncplan.ReasonParentConflict,
		"b.txt/deep":       "none:" + syncplan.ReasonParentConflict,
		"b.txt/deep/y.txt": "none:" + syncplan.ReasonParentConflict,
		"unrelated.txt":    "pull:" + syncplan.ReasonMissing,
	}
	for path, decision := range want {
		if got[path] != decision {
			t.Fatalf("plan[%s] = %q, want %q (plan = %v)", path, got[path], decision, got)
		}
	}
}
