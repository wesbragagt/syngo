// Package mergebase keeps the versions a three-way merge needs to merge
// against. A merge needs the content two concurrent edits both came from, and
// once a file has been edited over, nothing else on the device still holds it:
// so every version of a small text file this device records is kept, and the
// history is where a later divergence finds its base.
//
// Nothing here runs unless the text merge setting is on. With the setting off
// a Recorder is never built, no content is stored, and the disk cost is zero.
package mergebase

import (
	"context"
	"io"
	"log/slog"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/textmerge"
)

// Recorder writes merge base candidates. A nil Recorder records nothing, so a
// caller that has the setting off holds nil and calls it all the same.
type Recorder struct {
	store  ports.MergeBaseStore
	fs     ports.FileSystem
	hasher ports.Hasher
	clock  ports.Clock
	log    *slog.Logger
}

// New returns a recorder writing to store. log may be nil.
func New(store ports.MergeBaseStore, fsys ports.FileSystem, hasher ports.Hasher,
	clock ports.Clock, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Recorder{store: store, fs: fsys, hasher: hasher, clock: clock, log: log}
}

// Remember keeps the content of a version of one path, if it is a text file
// small enough to merge. The file is read back from disk and hashed, and
// nothing is kept unless it still hashes to what the entry says: a file that
// changed since it was recorded would otherwise be stored under the version of
// content it no longer holds, which is the one way this could feed a merge the
// wrong base.
//
// Failing to record a base is never an error the caller has to handle. The
// worst it costs is a later divergence that keeps both versions, which is what
// would have happened anyway.
func (r *Recorder) Remember(ctx context.Context, f folder.Folder, entry manifest.Entry) {
	if r == nil || entry.Kind != manifest.KindFile || entry.Deleted ||
		entry.Size > textmerge.MaxSize || entry.Hash.IsZero() {
		return
	}
	content, err := r.read(f.Path + "/" + entry.Path)
	if err != nil {
		r.log.Debug("merge base not recorded", "folder", f.ID, "path", entry.Path, "error", err)
		return
	}
	if !textmerge.IsText(content) {
		return
	}
	if r.hasher.SumBytes(content) != entry.Hash {
		// The file moved on between the scan and this read, so what was
		// read is not the version the entry describes.
		return
	}
	base := ports.MergeBase{
		FolderID: f.ID, Path: entry.Path, Version: entry.Version,
		Hash: entry.Hash, Content: content, SavedAt: r.clock.Now(),
	}
	if err := r.store.SaveMergeBase(ctx, base); err != nil {
		r.log.Debug("merge base not stored", "folder", f.ID, "path", entry.Path, "error", err)
	}
}

// read returns the whole content of a file, refusing anything above the merge
// limit so a file that grew since it was scanned cannot be read into memory.
func (r *Recorder) read(full string) ([]byte, error) {
	rc, err := r.fs.Open(full)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, textmerge.MaxSize+1))
}
