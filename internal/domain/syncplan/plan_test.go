package syncplan_test

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
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

// file returns a live file entry.
func file(path, content, synced string) manifest.Entry {
	return manifest.Entry{Path: path, Kind: manifest.KindFile, Size: 1, Hash: hash(content), SyncedHash: hash(synced)}
}

// modedFile returns a live file entry carrying permission bits and the mode
// both sides last agreed on.
func modedFile(path, content, synced string, mode, syncedMode uint32) manifest.Entry {
	e := file(path, content, synced)
	e.Mode = mode
	e.SyncedMode = syncedMode
	return e
}

// byDevice attributes an entry to the device that last wrote it.
func byDevice(e manifest.Entry, id device.ID) manifest.Entry {
	e.ModifiedBy = id
	return e
}

// tombstone returns a deleted entry.
func tombstone(path string) manifest.Entry {
	return manifest.Entry{Path: path, Kind: manifest.KindFile, Deleted: true}
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
	dir := manifest.Entry{Path: "sub", Kind: manifest.KindDir, Mode: 0o755}
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
			remote: manifestOf(file("a.txt", "v2", "")),
			action: syncplan.ActionPull, reason: syncplan.ReasonMissing,
		},
		{
			name:   "local tombstone is pulled again",
			local:  manifestOf(tombstone("a.txt")),
			remote: manifestOf(file("a.txt", "v2", "")),
			action: syncplan.ActionPull, reason: syncplan.ReasonMissing,
		},
		{
			name:   "same hash is in sync",
			local:  manifestOf(file("a.txt", "v1", "")),
			remote: manifestOf(file("a.txt", "v1", "")),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "remote changed and local untouched is pulled",
			local:  manifestOf(file("a.txt", "v1", "v1")),
			remote: manifestOf(file("a.txt", "v2", "")),
			action: syncplan.ActionPull, reason: syncplan.ReasonRemoteNewer,
		},
		{
			name:   "local changed and remote untouched is left alone",
			local:  manifestOf(file("a.txt", "v2", "v1")),
			remote: manifestOf(file("a.txt", "v1", "")),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "both changed is a conflict, never an overwrite",
			local:  manifestOf(file("a.txt", "local", "v1")),
			remote: manifestOf(file("a.txt", "remote", "")),
			action: syncplan.ActionConflict, reason: syncplan.ReasonBothModified,
		},
		{
			name: "a version the peer made on top of ours is pulled",
			// The local baseline is behind, but the peer says its
			// version was made on top of what this side holds, so
			// taking it loses nothing.
			local:  manifestOf(file("a.txt", "v2", "v1")),
			remote: manifestOf(file("a.txt", "v3", "v2")),
			action: syncplan.ActionPull, reason: syncplan.ReasonRemoteNewer,
		},
		{
			name: "an earlier version of our own change is not a conflict",
			// The peer holds a version this device wrote and has not
			// caught up with the newer one, which it pulls itself.
			local:  manifestOf(byDevice(file("a.txt", "v2", ""), "SELF")),
			remote: manifestOf(byDevice(file("a.txt", "v1", "v1"), "SELF")),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "two devices changing the same file is still a conflict",
			local:  manifestOf(byDevice(file("a.txt", "local", "v1"), "SELF")),
			remote: manifestOf(byDevice(file("a.txt", "remote", "v1"), "PEER")),
			action: syncplan.ActionConflict, reason: syncplan.ReasonBothModified,
		},
		{
			name:   "never synced and both sides have content is a conflict",
			local:  manifestOf(file("a.txt", "local", "")),
			remote: manifestOf(file("a.txt", "remote", "")),
			action: syncplan.ActionConflict, reason: syncplan.ReasonBothModified,
		},
		{
			name:   "remote deletion of an unchanged file is applied",
			local:  manifestOf(file("a.txt", "v1", "v1")),
			remote: manifestOf(tombstone("a.txt")),
			action: syncplan.ActionDelete, reason: syncplan.ReasonRemoteDeleted,
		},
		{
			name:   "remote deletion of a changed file is a conflict",
			local:  manifestOf(file("a.txt", "v2", "v1")),
			remote: manifestOf(tombstone("a.txt")),
			action: syncplan.ActionConflict, reason: syncplan.ReasonDeleteVsModify,
		},
		{
			name:   "missing directory is created",
			local:  manifestOf(),
			remote: manifestOf(dir),
			action: syncplan.ActionMkDir, reason: syncplan.ReasonMissing,
		},
		{
			name:   "existing directory is in sync",
			local:  manifestOf(dir),
			remote: manifestOf(dir),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "a file the peer turned into a directory is replaced",
			local:  manifestOf(file("f", "v1", "v1")),
			remote: manifestOf(manifest.Entry{Path: "f", Kind: manifest.KindDir, Mode: 0o755}),
			action: syncplan.ActionReplace, reason: syncplan.ReasonKindChanged,
		},
		{
			name:   "a locally modified file the peer turned into a directory is a conflict",
			local:  manifestOf(file("f", "local", "v1")),
			remote: manifestOf(manifest.Entry{Path: "f", Kind: manifest.KindDir, Mode: 0o755}),
			action: syncplan.ActionConflict, reason: syncplan.ReasonReplaceVsModify,
		},
		{
			name:   "a file the peer never synced and turned into a directory is a conflict",
			local:  manifestOf(file("f", "local", "")),
			remote: manifestOf(manifest.Entry{Path: "f", Kind: manifest.KindDir, Mode: 0o755}),
			action: syncplan.ActionConflict, reason: syncplan.ReasonReplaceVsModify,
		},
		{
			name:   "a directory the peer turned into a file is replaced",
			local:  manifestOf(dir),
			remote: manifestOf(file("sub", "v1", "")),
			action: syncplan.ActionReplace, reason: syncplan.ReasonKindChanged,
		},
		{
			name:   "a symlink the peer turned into a file is replaced",
			local:  manifestOf(manifest.Entry{Path: "link", Kind: manifest.KindSymlink, LinkTarget: "target"}),
			remote: manifestOf(file("link", "v1", "")),
			action: syncplan.ActionReplace, reason: syncplan.ReasonKindChanged,
		},
		{
			name:   "a mode the peer changed is applied",
			local:  manifestOf(modedFile("a.txt", "v1", "v1", 0o644, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", "", 0o700, 0)),
			action: syncplan.ActionChmod, reason: syncplan.ReasonModeChanged,
		},
		{
			name:   "a mode changed locally is left for the peer to take",
			local:  manifestOf(modedFile("a.txt", "v1", "v1", 0o700, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", "", 0o644, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "a mode without a baseline is not applied",
			local:  manifestOf(modedFile("a.txt", "v1", "v1", 0o644, 0)),
			remote: manifestOf(modedFile("a.txt", "v1", "", 0o700, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonModeUnknown,
		},
		{
			name:   "a mode is not applied to a locally modified file",
			local:  manifestOf(modedFile("a.txt", "v2", "v1", 0o644, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", "", 0o700, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonLocalNewer,
		},
		{
			name:   "a mode change waits for the content agreement to be recorded",
			local:  manifestOf(modedFile("a.txt", "v1", "", 0o644, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", "", 0o700, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "equal modes are in sync",
			local:  manifestOf(modedFile("a.txt", "v1", "v1", 0o644, 0o644)),
			remote: manifestOf(modedFile("a.txt", "v1", "", 0o644, 0)),
			action: syncplan.ActionNone, reason: syncplan.ReasonInSync,
		},
		{
			name:   "a peer that announces no mode changes nothing",
			local:  manifestOf(modedFile("a.txt", "v1", "v1", 0o644, 0o644)),
			remote: manifestOf(file("a.txt", "v1", "")),
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

	item := only(t, syncplan.Plan(manifestOf(file("a.txt", "v1", "v1")), manifestOf(tombstone("a.txt")), f))
	if item.Action != syncplan.ActionNone || item.Reason != syncplan.ReasonDeletePolicyNever {
		t.Fatalf("got %s(%s), want none(%s)", item.Action, item.Reason, syncplan.ReasonDeletePolicyNever)
	}
}

func TestPlan_SendOnlyProducesNothing(t *testing.T) {
	f := testFolder()
	f.Mode = folder.ModeSendOnly

	if items := syncplan.Plan(manifestOf(), manifestOf(file("a.txt", "v1", "")), f); len(items) != 0 {
		t.Fatalf("send-only folder planned %+v, want nothing", items)
	}
}

func TestPlan_RemoteTombstoneWithoutLocalEntryIsIgnored(t *testing.T) {
	if items := syncplan.Plan(manifestOf(), manifestOf(tombstone("a.txt")), testFolder()); len(items) != 0 {
		t.Fatalf("planned %+v for a path neither side has, want nothing", items)
	}
	local := manifestOf(tombstone("a.txt"))
	if items := syncplan.Plan(local, manifestOf(tombstone("a.txt")), testFolder()); len(items) != 0 {
		t.Fatalf("planned %+v for two tombstones, want nothing", items)
	}
}

func TestPlan_DirsBeforeFilesAndDeletesLast(t *testing.T) {
	local := manifestOf(file("old/gone.txt", "v1", "v1"), manifest.Entry{Path: "old", Kind: manifest.KindDir})
	remote := manifestOf(
		file("b/deep.txt", "v1", ""),
		file("a.txt", "v1", ""),
		manifest.Entry{Path: "b", Kind: manifest.KindDir},
		tombstone("old/gone.txt"),
		manifest.Entry{Path: "old", Kind: manifest.KindDir, Deleted: true},
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
		manifest.Entry{Path: "sub", Kind: manifest.KindDir},
		file("sub/inner.txt", "v1", "v1"),
	)
	remote := manifestOf(
		file("sub", "v2", ""),
		tombstone("sub/inner.txt"),
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
	local := manifestOf(file("f", "v1", "v1"))
	remote := manifestOf(
		file("f/inner.txt", "v2", ""),
		manifest.Entry{Path: "f", Kind: manifest.KindDir, Mode: 0o755},
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
