package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
	"github.com/wesbragagt/syncthis/internal/domain/textmerge"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// A three-way merge is the one thing the puller does that settles a conflict
// on its own, so every reason not to do it ends in the keep-both it replaced.
// It runs on the same side keep-both would have acted on, the side whose
// version gives up the path, and only there: the merged file dominates both
// versions, so the other side pulls it in its next cycle. Merging on both
// sides would mean two devices writing two files for one divergence.

// ErrNoMerge reports that a conflict was not merged. It is never returned to a
// caller that has to handle it: the puller keeps both versions instead, which
// is what it does with the setting off. The wrapped cause says why, for the
// log and for the tests.
var ErrNoMerge = errors.New("not merged")

// tryMerge settles a both-modified conflict by merging the two versions
// against the one they diverged from, and returns the entry the merged file
// leaves behind. Every doubt is an ErrNoMerge: content that is not text, a
// file too large, a base that cannot be shown to precede both versions, an
// overlap in the edits, or either version having moved on disk since the plan
// was made.
func (p *Puller) tryMerge(ctx context.Context, f folder.Folder, peer device.ID,
	item syncplan.Item, t transfer.Transfer) ([]arrived, error) {
	if p.bases == nil {
		return nil, fmt.Errorf("%w: the text merge setting is off", ErrNoMerge)
	}
	if !mergeable(item) {
		return nil, fmt.Errorf("%w: %s is not a pair of text-sized files", ErrNoMerge, item.Path)
	}
	local, err := p.contentOf(f.Path+"/"+item.Path, item.Local.Hash)
	if err != nil {
		return nil, fmt.Errorf("%w: local version of %s: %w", ErrNoMerge, item.Path, err)
	}
	remote, err := p.contentOf(t.TempPath, item.Remote.Hash)
	if err != nil {
		return nil, fmt.Errorf("%w: peer's version of %s: %w", ErrNoMerge, item.Path, err)
	}
	base, err := p.mergeBase(ctx, f.ID, item)
	if err != nil {
		return nil, err
	}

	merged, err := textmerge.Merge(base.Content, local, remote)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoMerge, item.Path, err)
	}

	entry, err := p.installMerged(ctx, f, item, t, merged)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoMerge, item.Path, err)
	}
	p.log.Info("conflict merged", "folder", f.ID, "path", item.Path, "peer", peer.Short(),
		"base_version", base.Version.String(), "bytes", len(merged))
	p.publish("sync.merged", map[string]any{
		"folder_id": string(f.ID), "path": item.Path, "peer_id": string(peer),
		"base_version": base.Version.String(), "bytes": int64(len(merged)),
	})
	// The merged file holds both edits, so it descends from both versions
	// and dominates them; this device wrote it, so it carries its counter
	// too. The peer takes it as an ordinary newer version.
	return []arrived{{
		entry:   entry,
		author:  p.self(),
		version: item.Local.Version.Merge(item.Remote.Version).Bump(p.self()),
		expect:  plannedVersion(item.Local),
	}}, nil
}

// mergeable reports whether a plan item is a conflict between two versions of
// one file that are both small enough to merge. Anything else, a deletion or a
// kind change included, has no two texts to merge.
func mergeable(item syncplan.Item) bool {
	return item.Reason == syncplan.ReasonBothModified &&
		item.Local != nil && item.Remote != nil &&
		item.Local.Kind == manifest.KindFile && item.Remote.Kind == manifest.KindFile &&
		!item.Local.Deleted && !item.Remote.Deleted &&
		item.Local.Size <= textmerge.MaxSize && item.Remote.Size <= textmerge.MaxSize
}

// mergeBase returns the version both sides of a conflict descend from. It is
// the newest recorded version of the path that both of them dominate: a
// candidate neither has moved past is not an ancestor of both, and merging
// against it could drop an edit. A path with no such candidate is not merged.
func (p *Puller) mergeBase(ctx context.Context, id folder.ID, item syncplan.Item) (ports.MergeBase, error) {
	candidates, err := p.store.MergeBases(ctx, id, item.Path)
	if err != nil {
		return ports.MergeBase{}, fmt.Errorf("%w: merge bases of %s: %w", ErrNoMerge, item.Path, err)
	}
	for _, base := range candidates {
		if base.Version.Compare(item.Local.Version) == manifest.Before &&
			base.Version.Compare(item.Remote.Version) == manifest.Before {
			return base, nil
		}
	}
	return ports.MergeBase{}, fmt.Errorf("%w: no recorded version of %s precedes both sides",
		ErrNoMerge, item.Path)
}

// contentOf reads a whole file and returns it only if it still hashes to want.
// A file that moved on since the plan was made is not one of the two versions
// being merged, and merging what is there instead would lose whatever changed
// it.
func (p *Puller) contentOf(full string, want manifest.Hash) ([]byte, error) {
	rc, err := p.fs.Open(full)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	content, err := io.ReadAll(io.LimitReader(rc, textmerge.MaxSize+1))
	if err != nil {
		return nil, err
	}
	if got := p.hasher.SumBytes(content); got != want {
		return nil, fmt.Errorf("holds %s, expected %s: %w", got, want, ErrChangedUnderneath)
	}
	return content, nil
}

// installMerged writes the merged content to a partial file and puts it at the
// path through the ordinary installer, so the same atomic rename and the same
// check against the local version the plan was made against apply. The
// downloaded partial file is dropped: its content is in the merge.
func (p *Puller) installMerged(ctx context.Context, f folder.Folder, item syncplan.Item,
	t transfer.Transfer, merged []byte) (manifest.Entry, error) {
	temp, err := p.newTemp(f.Path, t.ID)
	if err != nil {
		return manifest.Entry{}, err
	}
	if err := p.writeTemp(temp, merged); err != nil {
		p.removeTemp(temp)
		return manifest.Entry{}, err
	}
	// The merged file is this device's own new version, so it carries the
	// time it was made and the mode the local version had.
	now := p.clock.Now()
	entry, err := p.installer.Install(ctx, f.Path, transfer.Transfer{
		FolderID: f.ID, Path: item.Path, PeerID: t.PeerID,
		Hash: p.hasher.SumBytes(merged), Size: int64(len(merged)),
		MTimeNS: now.UnixNano(), Mode: item.Local.Mode, TempPath: temp,
	}, item.Local)
	if err != nil {
		p.removeTemp(temp)
		return manifest.Entry{}, err
	}
	p.removeTemp(t.TempPath)
	return entry, nil
}

// writeTemp fills a partial file with content and flushes it.
func (p *Puller) writeTemp(temp string, content []byte) error {
	file, err := p.fs.OpenTemp(temp)
	if err != nil {
		return fmt.Errorf("open %s: %w", temp, err)
	}
	if _, err := file.WriteAt(content, 0); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", temp, err)
	}
	if err := file.Truncate(int64(len(content))); err != nil {
		_ = file.Close()
		return fmt.Errorf("truncate %s: %w", temp, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("flush %s: %w", temp, err)
	}
	return file.Close()
}

// recorded is the entry as the manifest now holds it: what was installed,
// under the version and author the record gave it. It is what a merge base is
// stored against, because the version is how a later divergence recognizes the
// content as an ancestor of both sides.
func recorded(a arrived) manifest.Entry {
	e := a.entry
	e.ModifiedBy = a.author
	e.Version = a.version
	return e
}
