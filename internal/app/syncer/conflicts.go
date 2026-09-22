package syncer

import (
	"context"
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/conflict"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// Conflicts are kept, never resolved by the puller: whatever it does, both
// versions are still on disk afterwards.
//
// Both sides of a "both-modified" conflict see the same two hashes, and
// conflict.KeepsPath turns them into the same answer about which version keeps
// the path. The side whose version loses is the only one that acts: it renames
// its file to a conflict name and installs the peer's version at the path.
// The copy it wrote is an ordinary file from then on, so the peer pulls it in
// the next cycle and both trees end up with the same two files. The side whose
// version wins does nothing but say so; acting on both sides would make the
// two devices swap versions forever instead of converging.

// needsRemoteContent reports whether a conflict is settled by downloading the
// peer's version, which makes it a member of the pull phase rather than of
// apply. That is the case for exactly one conflict: both sides changed a file
// and the local version is the one that gives up the path. A path the peer
// deleted or turned into a directory has no content to fetch.
func needsRemoteContent(item syncplan.Item) bool {
	return item.Action == syncplan.ActionConflict && item.Reason == syncplan.ReasonBothModified &&
		item.Local != nil && item.Remote != nil && item.Remote.Kind == manifest.KindFile &&
		!conflict.KeepsPath(item.Local.Hash, item.Remote.Hash)
}

// keepBoth moves the local version aside and installs the peer's version in
// its place, returning the entries that now describe the folder: the file at
// the path, and the copy holding what this device had.
func (p *Puller) keepBoth(ctx context.Context, f folder.Folder, peer device.ID,
	item syncplan.Item, t transfer.Transfer) ([]arrived, error) {
	// The copy is named after the version it holds, not after this meeting
	// of two devices: its author and its modification time. Two devices
	// moving the same losing version aside therefore write one name, so
	// the copies converge instead of multiplying with every pair.
	name := conflict.Name(item.Path, item.Local.ModTime, p.authorOfLocal(*item.Local))
	// The local version moves aside first, so the path is free for the
	// peer's version and the local bytes are never overwritten.
	kept, err := p.installer.KeepLocalAs(ctx, f.Path, item.Path, name, item.Local)
	if err != nil {
		return nil, err
	}
	// A reused copy leaves the local file at its path, so the install
	// replaces it and is told which version it is replacing.
	var expect *manifest.Entry
	if kept.Reused {
		expect = item.Local
	}
	installed, err := p.installer.Install(ctx, f.Path, t, expect)
	if err != nil {
		if kept.Reused {
			// Nothing was moved: the local version is still at its
			// path and the copy beside it is untouched.
			return nil, err
		}
		// The path is empty now and the local version sits at a name
		// nothing recorded, which the next scan would read as the path
		// having been deleted. It goes back where it was.
		if restoreErr := p.installer.RestoreKept(ctx, f.Path, kept.Entry.Path, item.Path); restoreErr != nil {
			p.log.Error("local version left at its conflict name", "folder", f.ID,
				"path", item.Path, "conflict_path", kept.Entry.Path, "error", restoreErr)
			// It cannot go back, so the manifest is told where it
			// is: an unrecorded file would be read as a deletion of
			// the path and as an addition beside it.
			if recordErr := p.record(ctx, f.ID, p.keptCopy(ctx, f.ID, kept.Entry, item)); recordErr != nil {
				return nil, errors.Join(err, restoreErr, recordErr)
			}
			return nil, errors.Join(err, restoreErr)
		}
		return nil, err
	}
	if err := p.recordConflict(ctx, f, peer, item, kept.Entry.Path); err != nil {
		return nil, err
	}
	// The version at the path is the outcome of the conflict, so it
	// dominates both inputs: a third device holding either takes it
	// without conflicting a second time.
	outcome := arrived{
		entry:   installed,
		author:  authorOf(*item.Remote, peer),
		version: item.Local.Version.Merge(item.Remote.Version),
	}
	return []arrived{outcome, p.keptCopy(ctx, f.ID, kept.Entry, item)}, nil
}

// keptCopy describes the copy holding the version that gave up the path. It
// keeps the author of that version, and its vector is raised from whatever the
// copy's path held here before, a tombstone of an earlier copy included, so
// the copy is a new version of its own path rather than a stale one.
func (p *Puller) keptCopy(ctx context.Context, id folder.ID, kept manifest.Entry, item syncplan.Item) arrived {
	var prev manifest.Version
	if stored, err := p.store.GetFile(ctx, id, kept.Path); err == nil {
		prev = stored.Version
	}
	return arrived{
		entry:   kept,
		author:  p.authorOfLocal(*item.Local),
		version: prev.Bump(p.self()),
	}
}

// authorOfLocal is the device that produced the local version of a path: the
// one recorded for it, or this device when the entry predates authorship.
func (p *Puller) authorOfLocal(local manifest.Entry) device.ID {
	if local.ModifiedBy == "" {
		return p.self()
	}
	return local.ModifiedBy
}

// authorOf is the device that produced a remote version: the one the peer
// announced, or the peer itself when it announced none.
func authorOf(remote manifest.Entry, peer device.ID) device.ID {
	if remote.ModifiedBy == "" {
		return peer
	}
	return remote.ModifiedBy
}

// conflictKept reports a conflict that leaves the local side exactly as it is:
// the peer deleted a file this device changed, the peer replaced it by another
// kind, or the local version is the one that keeps the path. A conflict record
// is written for the first two, because this device then holds a version the
// peer does not have and the user has to say what should happen to it. The
// third needs none: the peer keeps both versions and sends the copy here.
func (p *Puller) conflictKept(ctx context.Context, f folder.Folder, peer device.ID,
	item syncplan.Item) error {
	p.log.Warn("conflict: both versions are kept", "folder", f.ID, "path", item.Path,
		"reason", item.Reason)
	if item.Reason == syncplan.ReasonBothModified {
		p.publish("sync.conflict", map[string]any{
			"folder_id": string(f.ID), "path": item.Path, "reason": item.Reason,
			"peer_id": string(peer), "conflict_path": "",
		})
		return nil
	}
	return p.recordConflict(ctx, f, peer, item, "")
}

// recordConflict writes the conflict row behind `syncthis conflict list` and
// publishes the event. The row is identified by what diverged, so a conflict
// the sync plan reports once per cycle updates one row instead of adding one.
func (p *Puller) recordConflict(ctx context.Context, f folder.Folder, peer device.ID,
	item syncplan.Item, conflictPath string) error {
	var local, remote manifest.Hash
	if item.Local != nil {
		local = item.Local.Hash
	}
	if item.Remote != nil {
		remote = item.Remote.Hash
	}
	record := ports.ConflictRecord{
		ID:           conflict.RecordID(f.ID, item.Path, local, remote, peer),
		FolderID:     f.ID,
		Path:         item.Path,
		ConflictPath: conflictPath,
		LocalHash:    local,
		RemoteHash:   remote,
		PeerID:       peer,
		DetectedAt:   p.clock.Now(),
		Reason:       item.Reason,
	}
	if err := p.store.SaveConflict(ctx, record); err != nil {
		return fmt.Errorf("record conflict on %q: %w", item.Path, err)
	}
	p.log.Warn("conflict recorded", "folder", f.ID, "path", item.Path,
		"reason", item.Reason, "conflict_path", conflictPath, "conflict", record.ID)
	p.publish("sync.conflict", map[string]any{
		"folder_id": string(f.ID), "path": item.Path, "reason": item.Reason,
		"peer_id": string(peer), "conflict_id": record.ID, "conflict_path": conflictPath,
	})
	return nil
}
