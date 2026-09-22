// Package syncplan decides, for a folder, which files must be pulled, deleted
// or left alone after comparing the local and remote manifests.
//
// The comparison is by content hash, never by timestamp. Every decision that
// would overwrite or remove a locally modified file is reported as a conflict
// instead, so a plan can never lose local data on its own.
package syncplan

import (
	"sort"
	"strings"

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
	// ActionReplace means the local entry is of another kind than the
	// remote one and must be removed before the remote kind is created.
	ActionReplace
	// ActionChmod means only the permission bits differ and must be set to
	// the remote ones; no content is transferred.
	ActionChmod
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
	case ActionReplace:
		return "replace"
	case ActionChmod:
		return "chmod"
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
	ReasonKindChanged       = "kind-changed"
	ReasonReplaceVsModify   = "replace-vs-modify"
	ReasonModeChanged       = "mode-changed"
	ReasonModeUnknown       = "mode-baseline-unknown"
	ReasonInvalidPath       = "invalid-path"
	ReasonParentConflict    = "parent-conflict"
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
// path order, so parents exist before their children. Deletions follow,
// deepest path first, so a directory is removed after its contents. Replacing
// a directory with a file or a symlink comes last of all, because the
// directory can only go once the deletions emptied it.
//
// A folder that does not receive produces no items at all.
func Plan(local, remote manifest.Manifest, f folder.Folder) []Item {
	if !f.CanReceive() {
		return nil
	}

	var creates, deletes, replaces []Item
	// Paths the peer holds as a directory while a local file of the same
	// name is in conflict with it. Nothing can be written beneath such a
	// path until the conflict is settled. remote.Sorted is in path order,
	// so a parent is always decided before its children.
	var blocked []string
	for _, r := range remote.Sorted() {
		entry := r
		var localEntry *manifest.Entry
		if l, ok := local.Entries[r.Path]; ok {
			cp := l
			localEntry = &cp
		}

		item := decide(localEntry, &entry, f)
		if under(r.Path, blocked) {
			item = with(item, ActionNone, ReasonParentConflict)
		}
		if item.Action == ActionConflict && item.Reason == ReasonReplaceVsModify {
			blocked = append(blocked, r.Path)
		}
		switch {
		case item.Action == ActionDelete:
			deletes = append(deletes, item)
		case item.Action == ActionReplace && item.Remote.Kind != manifest.KindDir:
			replaces = append(replaces, item)
		case item.Action == ActionNone && item.Reason == "":
		default:
			creates = append(creates, item)
		}
	}

	sort.SliceStable(creates, func(i, j int) bool {
		a, b := creates[i], creates[j]
		if dirFirst(a) != dirFirst(b) {
			return dirFirst(a)
		}
		return a.Path < b.Path
	})
	sort.SliceStable(deletes, func(i, j int) bool { return deletes[i].Path > deletes[j].Path })
	sort.SliceStable(replaces, func(i, j int) bool { return replaces[i].Path < replaces[j].Path })
	out := append(creates, deletes...)
	return append(out, replaces...)
}

// decide applies the comparison rules to one path. An item with ActionNone and
// an empty reason means "nothing to record at all".
func decide(local, remote *manifest.Entry, f folder.Folder) Item {
	item := Item{Path: remote.Path, Local: local, Remote: remote}

	// A path the peer may not name is skipped rather than acted on. The
	// wire layer already refuses such a manifest; this is the second of
	// the three places that check, so a plan is safe whatever built it.
	if err := manifest.ValidRelPath(remote.Path); err != nil {
		return with(item, ActionNone, ReasonInvalidPath)
	}
	if remote.Kind == manifest.KindSymlink && !remote.Deleted {
		if err := manifest.ValidLinkTarget(remote.Path, remote.LinkTarget); err != nil {
			return with(item, ActionNone, ReasonInvalidPath)
		}
	}

	if remote.Deleted {
		return remoteDeleted(item, local, remote, f)
	}
	if local != nil && !local.Deleted && local.Kind != remote.Kind {
		if remote.Kind == manifest.KindSymlink && f.Symlinks != folder.SymlinkStore {
			return with(item, ActionNone, ReasonSymlinkPolicy)
		}
		return kindChanged(item, local, remote)
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

// remoteFile decides what to do with a file the peer has. Two live versions
// are ordered by their version vectors alone: the one that dominates is the
// newer one, and two that neither dominate were written independently, which
// is a conflict.
func remoteFile(item Item, local, remote *manifest.Entry) Item {
	switch {
	case local == nil:
		return with(item, ActionPull, ReasonMissing)
	case local.Deleted:
		// A deletion made after the peer's version keeps the path
		// deleted; the peer takes the tombstone itself. Anything else,
		// a concurrent deletion included, keeps the content: a file
		// that comes back can be deleted again, one that is gone is
		// gone.
		if local.Version.Compare(remote.Version) == manifest.After {
			return with(item, ActionNone, ReasonLocalNewer)
		}
		return with(item, ActionPull, ReasonMissing)
	case local.Kind == manifest.KindFile && local.Hash == remote.Hash:
		return sameContent(item, local, remote)
	}
	switch local.Version.Compare(remote.Version) {
	case manifest.Before:
		return with(item, ActionPull, ReasonRemoteNewer)
	case manifest.After:
		return with(item, ActionNone, ReasonLocalNewer)
	default:
		// Concurrent, and Equal with different content: the second only
		// happens for rows that predate version vectors, where the
		// empty vector says nothing about which version came first.
		// Keeping both is the only answer that loses nothing.
		return with(item, ActionConflict, ReasonBothModified)
	}
}

// sameContent decides what to do with a file whose content both sides agree
// on. Only the permission bits can still differ, and they are followed just
// like content is: the side that did not change them takes the other side's.
func sameContent(item Item, local, remote *manifest.Entry) Item {
	localMode, remoteMode := perm(local.Mode), perm(remote.Mode)
	switch {
	case remoteMode == 0 || localMode == remoteMode:
		// In sync. The caller merges the two version vectors, so the
		// shared history of the path is recorded even though nothing
		// moves, and records the mode as the new baseline.
		return with(item, ActionNone, ReasonInSync)
	case local.SyncedMode == 0:
		// No baseline: the entry predates mode tracking, so which side
		// changed cannot be told. Waiting is the only safe answer.
		return with(item, ActionNone, ReasonModeUnknown)
	case localMode != perm(local.SyncedMode):
		// Only the local side changed the mode; the peer will follow it.
		return with(item, ActionNone, ReasonLocalNewer)
	default:
		return with(item, ActionChmod, ReasonModeChanged)
	}
}

// kindChanged decides what to do with a path that is a file on one side and a
// directory or a symlink on the other. The local entry is only replaced when
// the peer's version was made on top of it: a file changed independently of
// the replacement is a conflict, because replacing it would lose the change.
func kindChanged(item Item, local, remote *manifest.Entry) Item {
	if local.Kind != manifest.KindFile {
		// Directories and symlinks carry no content of their own, so
		// there is nothing a replacement could lose.
		return with(item, ActionReplace, ReasonKindChanged)
	}
	switch local.Version.Compare(remote.Version) {
	case manifest.After:
		return with(item, ActionNone, ReasonLocalNewer)
	case manifest.Concurrent:
		return with(item, ActionConflict, ReasonReplaceVsModify)
	default:
		return with(item, ActionReplace, ReasonKindChanged)
	}
}

// perm returns the permission bits of a stored mode.
func perm(mode uint32) uint32 { return mode & 0o7777 }

// remoteDeleted decides what to do with a path the peer deleted. A file only
// follows the deletion when the deletion was made on top of the version this
// side holds; a local change the peer never saw is a conflict, never a loss.
func remoteDeleted(item Item, local, remote *manifest.Entry, f folder.Folder) Item {
	switch {
	case local == nil || local.Deleted:
		return Item{Path: item.Path, Local: local, Remote: item.Remote}
	case f.DeletePolicy == folder.DeleteNever:
		return with(item, ActionNone, ReasonDeletePolicyNever)
	case local.Kind != manifest.KindFile:
		// Directories and symlinks hold nothing a deletion could lose;
		// a directory that still has content is kept by the installer,
		// not by the plan.
		return with(item, ActionDelete, ReasonRemoteDeleted)
	}
	switch local.Version.Compare(remote.Version) {
	case manifest.Before:
		return with(item, ActionDelete, ReasonRemoteDeleted)
	case manifest.After:
		return with(item, ActionNone, ReasonLocalNewer)
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

// under reports whether p lies beneath one of the paths.
func under(p string, paths []string) bool {
	for _, parent := range paths {
		if strings.HasPrefix(p, parent+"/") {
			return true
		}
	}
	return false
}

// dirFirst reports whether an item must be applied before file items.
func dirFirst(i Item) bool {
	return i.Remote != nil && i.Remote.Kind == manifest.KindDir
}

// LocalSources pairs pull items with a local file that already holds exactly
// the content they need, so the receiver copies it instead of asking the peer
// for the bytes. The result maps the path of such an item to the
// folder-relative path of the file holding the content.
//
// This is the receiving half of rename detection: a peer that moved a file
// announces a new path whose content this device already has under the old
// one. Nothing here is a guarantee that the content is still on disk under
// that name, only that the manifest says so; the caller reads the file and
// hashes it before it uses it, and downloads normally when it does not match.
//
// Only pulls of non-empty files with a known hash take part, and only a
// candidate whose kind, size and hash match the remote entry exactly. When
// several local files hold the content, the lowest path is chosen, so two runs
// of the same plan pick the same source.
func LocalSources(local manifest.Manifest, items []Item) map[string]string {
	byContent := make(map[contentKey]string)
	for _, e := range local.Live() {
		if !contentSource(e) {
			continue
		}
		key := contentKey{e.Hash, e.Size}
		if first, ok := byContent[key]; !ok || e.Path < first {
			byContent[key] = e.Path
		}
	}
	if len(byContent) == 0 {
		return nil
	}

	var out map[string]string
	for _, item := range items {
		if item.Action != ActionPull || item.Remote == nil || !contentSource(*item.Remote) {
			continue
		}
		source, ok := byContent[contentKey{item.Remote.Hash, item.Remote.Size}]
		if !ok || source == item.Path {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[item.Path] = source
	}
	return out
}

// contentKey identifies content a file holds, whatever its name.
type contentKey struct {
	hash manifest.Hash
	size int64
}

// contentSource reports whether an entry describes content another path can be
// satisfied from.
func contentSource(e manifest.Entry) bool {
	return e.Kind == manifest.KindFile && !e.Deleted && e.Size > 0 && !e.Hash.IsZero()
}
