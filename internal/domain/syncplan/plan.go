// Package syncplan decides, for a folder, which files must be pulled, deleted
// or left alone after comparing the local and remote manifests.
//
// The comparison is by content hash, never by timestamp. Every decision that
// would overwrite or remove a locally modified file is reported as a conflict
// instead, so a plan can never lose local data on its own.
package syncplan

import (
	"sort"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Action is what a sync plan says to do with one path.
type Action uint8

// Plan actions.
const (
	// ActionNone means local and remote agree.
	ActionNone Action = iota
	// ActionPull means the remote version must be downloaded.
	ActionPull
	// ActionDelete means the local version must be removed.
	ActionDelete
	// ActionConflict means both sides changed and the versions differ.
	ActionConflict
	// ActionMkDir means the directory must be created locally.
	ActionMkDir
	// ActionSymlink means the symbolic link must be created locally.
	ActionSymlink
)

// String returns the lowercase name of the action.
func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionPull:
		return "pull"
	case ActionDelete:
		return "delete"
	case ActionConflict:
		return "conflict"
	case ActionMkDir:
		return "mkdir"
	case ActionSymlink:
		return "symlink"
	default:
		return "unknown"
	}
}

// Reasons explaining why an action was chosen. They are stable strings, used
// in logs and in the dry-run output.
const (
	ReasonInSync            = "in-sync"
	ReasonMissing           = "missing-locally"
	ReasonRemoteNewer       = "remote-newer"
	ReasonLocalNewer        = "local-newer"
	ReasonBothModified      = "both-modified"
	ReasonDeleteVsModify    = "delete-vs-modify"
	ReasonDeletePolicyNever = "delete-policy-never"
	ReasonSymlinkPolicy     = "symlink-policy-skip"
	ReasonRemoteDeleted     = "remote-deleted"
)

// Item is one decision in a plan, describing what to do with Path.
type Item struct {
	Action Action
	Path   string
	Local  *manifest.Entry
	Remote *manifest.Entry
	Reason string
}

// Plan compares the remote manifest with the local one and returns what the
// local side must do to converge. Only remote entries produce items: local
// entries the remote does not know about are the peer's business, and the peer
// pulls them itself.
//
// Items that create content come first, directories before files and both in
// path order, so parents exist before their children. Deletions come last,
// deepest path first, so a directory is removed after its contents.
//
// A folder that does not receive produces no items at all.
func Plan(local, remote manifest.Manifest, f folder.Folder) []Item {
	if !f.CanReceive() {
		return nil
	}

	var creates, deletes []Item
	for _, r := range remote.Sorted() {
		entry := r
		var localEntry *manifest.Entry
		if l, ok := local.Entries[r.Path]; ok {
			cp := l
			localEntry = &cp
		}

		item := decide(localEntry, &entry, f)
		if item.Action == ActionDelete {
			deletes = append(deletes, item)
			continue
		}
		if item.Action == ActionNone && item.Reason == "" {
			continue
		}
		creates = append(creates, item)
	}

	sort.SliceStable(creates, func(i, j int) bool {
		a, b := creates[i], creates[j]
		if dirFirst(a) != dirFirst(b) {
			return dirFirst(a)
		}
		return a.Path < b.Path
	})
	sort.SliceStable(deletes, func(i, j int) bool { return deletes[i].Path > deletes[j].Path })
	return append(creates, deletes...)
}

// decide applies the comparison rules to one path. An item with ActionNone and
// an empty reason means "nothing to record at all".
func decide(local, remote *manifest.Entry, f folder.Folder) Item {
	item := Item{Path: remote.Path, Local: local, Remote: remote}

	if remote.Deleted {
		return remoteDeleted(item, local, f)
	}
	switch remote.Kind {
	case manifest.KindDir:
		if local != nil && !local.Deleted && local.Kind == manifest.KindDir {
			return with(item, ActionNone, ReasonInSync)
		}
		return with(item, ActionMkDir, ReasonMissing)
	case manifest.KindSymlink:
		if f.Symlinks != folder.SymlinkStore {
			return with(item, ActionNone, ReasonSymlinkPolicy)
		}
		if local != nil && !local.Deleted && local.Kind == manifest.KindSymlink &&
			local.LinkTarget == remote.LinkTarget {
			return with(item, ActionNone, ReasonInSync)
		}
		return with(item, ActionSymlink, ReasonMissing)
	default:
		return remoteFile(item, local, remote)
	}
}

// remoteFile decides what to do with a file the peer has.
func remoteFile(item Item, local, remote *manifest.Entry) Item {
	switch {
	case local == nil || local.Deleted:
		return with(item, ActionPull, ReasonMissing)
	case local.Kind == manifest.KindFile && local.Hash == remote.Hash:
		return with(item, ActionNone, ReasonInSync)
	case local.Kind == manifest.KindFile && local.Hash == local.SyncedHash:
		// The local copy is still the one both sides agreed on, so the
		// remote version is the newer one.
		return with(item, ActionPull, ReasonRemoteNewer)
	case local.Kind == manifest.KindFile && remote.Hash == local.SyncedHash:
		// Only the local side moved on; the peer will pull it.
		return with(item, ActionNone, ReasonLocalNewer)
	default:
		return with(item, ActionConflict, ReasonBothModified)
	}
}

// remoteDeleted decides what to do with a path the peer deleted.
func remoteDeleted(item Item, local *manifest.Entry, f folder.Folder) Item {
	switch {
	case local == nil || local.Deleted:
		return Item{Path: item.Path, Local: local, Remote: item.Remote}
	case f.DeletePolicy == folder.DeleteNever:
		return with(item, ActionNone, ReasonDeletePolicyNever)
	case local.Kind != manifest.KindFile || local.Hash == local.SyncedHash:
		// Directories and symlinks carry no hash, so they follow the
		// peer; a file only follows it when it is unchanged since the
		// last time both sides agreed.
		return with(item, ActionDelete, ReasonRemoteDeleted)
	default:
		return with(item, ActionConflict, ReasonDeleteVsModify)
	}
}

// with fills in the action and reason of an item.
func with(item Item, a Action, reason string) Item {
	item.Action = a
	item.Reason = reason
	return item
}

// dirFirst reports whether an item must be applied before file items.
func dirFirst(i Item) bool {
	return i.Remote != nil && i.Remote.Kind == manifest.KindDir
}
