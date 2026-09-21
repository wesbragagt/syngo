package syncer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/conflict"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// TempDirName is the folder-relative directory holding partial downloads. It
// lives inside the folder so a finished download is renamed over its target
// without crossing a filesystem boundary.
const TempDirName = ".syncthis/tmp"

// maxConflictNames is how many counted variants of a conflict name are tried
// before the install gives up. A directory that holds them all has a problem
// no further copy would solve.
const maxConflictNames = 100

// Default permissions used when a peer announces none.
const (
	defaultFileMode fs.FileMode = 0o644
	defaultDirMode  fs.FileMode = 0o755
)

// Installation errors.
var (
	// ErrIntegrity reports content that does not hash to what was expected.
	// Nothing is installed and the partial file is kept for inspection.
	ErrIntegrity = errors.New("hash verification failed")
	// ErrChangedUnderneath reports that the local file moved while it was
	// being downloaded. The download is discarded rather than the local
	// change.
	ErrChangedUnderneath = errors.New("local file changed during the transfer")
	// ErrDirNotEmpty reports a directory the peer replaced by something else
	// that still holds entries the peer does not know about. It is kept, and
	// only that one path is skipped.
	ErrDirNotEmpty = errors.New("directory is not empty")
)

// Installer applies finished downloads and plan decisions to the filesystem.
type Installer struct {
	fs     ports.FileSystem
	hasher ports.Hasher
}

// NewInstaller returns an installer writing through fsys.
func NewInstaller(fsys ports.FileSystem, hasher ports.Hasher) *Installer {
	return &Installer{fs: fsys, hasher: hasher}
}

// TempDir returns the directory holding the partial downloads of a folder.
func (i *Installer) TempDir(root string) string { return path.Join(root, TempDirName) }

// Install verifies the finished partial file of t and moves it over its
// target. expectLocal describes the local version the plan was made against;
// a target that no longer matches it is left alone, because the local change
// would otherwise be lost. It returns the entry that now describes the file.
func (i *Installer) Install(_ context.Context, root string, t transfer.Transfer,
	expectLocal *manifest.Entry) (manifest.Entry, error) {
	return i.installAt(root, t.Path, t, expectLocal)
}

// KeepLocalAs moves the local file at rel aside to a conflict name, so the
// peer's version can take its place without the local change being lost. The
// local file must still be the version the plan was made against; one that
// changed since is left alone. A conflict name that is taken gets a counter.
// It returns the entry describing the copy, whose Path is the name it got.
func (i *Installer) KeepLocalAs(_ context.Context, root, rel, conflictRel string,
	local *manifest.Entry) (manifest.Entry, error) {
	full := path.Join(root, rel)
	info, err := i.fs.Lstat(full)
	if err != nil {
		return manifest.Entry{}, fmt.Errorf("stat %s: %w", full, err)
	}
	if local == nil || local.Kind != manifest.KindFile || info.IsDir() || info.IsSymlink() ||
		info.Size != local.Size || info.ModTime.UnixNano() != local.ModTime.UnixNano() {
		return manifest.Entry{}, fmt.Errorf("%s: %w", full, ErrChangedUnderneath)
	}

	free, err := i.freeName(root, conflictRel)
	if err != nil {
		return manifest.Entry{}, err
	}
	kept := path.Join(root, free)
	if err := i.fs.MkdirAll(path.Dir(kept), defaultDirMode); err != nil {
		return manifest.Entry{}, fmt.Errorf("create parent of %s: %w", kept, err)
	}
	// The rename is atomic, so the local version is either at its path or
	// at the conflict name, never nowhere.
	if err := i.fs.Rename(full, kept); err != nil {
		return manifest.Entry{}, fmt.Errorf("keep %s as %s: %w", full, kept, err)
	}
	if err := i.fs.SyncDir(path.Dir(kept)); err != nil {
		return manifest.Entry{}, fmt.Errorf("flush directory of %s: %w", kept, err)
	}

	entry := manifest.Entry{
		Path: free, Kind: manifest.KindFile, Size: local.Size, ModTime: local.ModTime,
		Mode: local.Mode, Hash: local.Hash, SyncedHash: local.Hash,
	}
	return i.described(kept, entry), nil
}

// freeName returns rel, or the first counted variant of it that no file holds.
func (i *Installer) freeName(root, rel string) (string, error) {
	candidate := rel
	for n := 1; n <= maxConflictNames; n++ {
		_, err := i.fs.Lstat(path.Join(root, candidate))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return candidate, nil
		case err != nil:
			return "", fmt.Errorf("stat %s: %w", path.Join(root, candidate), err)
		}
		candidate = conflict.WithCounter(rel, n)
	}
	return "", fmt.Errorf("%s: no free name left beside it: %w", rel, ErrChangedUnderneath)
}

// installAt verifies the finished partial file of t and moves it to rel.
func (i *Installer) installAt(root, rel string, t transfer.Transfer,
	expectLocal *manifest.Entry) (manifest.Entry, error) {
	hash, err := i.hashFile(t.TempPath)
	if err != nil {
		return manifest.Entry{}, err
	}
	if hash != t.Hash {
		return manifest.Entry{}, fmt.Errorf("%s: downloaded %s, expected %s: %w",
			t.Path, hash, t.Hash, ErrIntegrity)
	}

	final := path.Join(root, rel)
	if err := i.checkTarget(final, expectLocal); err != nil {
		return manifest.Entry{}, err
	}

	mode := fs.FileMode(t.Mode).Perm()
	if mode == 0 {
		mode = defaultFileMode
	}
	if err := i.fs.MkdirAll(path.Dir(final), defaultDirMode); err != nil {
		return manifest.Entry{}, fmt.Errorf("create parent of %s: %w", final, err)
	}
	if err := i.fs.Chmod(t.TempPath, mode); err != nil {
		return manifest.Entry{}, fmt.Errorf("set mode of %s: %w", t.TempPath, err)
	}
	mtime := time.Unix(0, t.MTimeNS)
	if err := i.fs.Chtimes(t.TempPath, mtime); err != nil {
		return manifest.Entry{}, fmt.Errorf("set modification time of %s: %w", t.TempPath, err)
	}
	// The content and the metadata of the partial file reach the disk
	// before the rename, and the directory entry reaches it after, so a
	// crash either leaves the old file or the complete new one.
	if err := i.sync(t.TempPath); err != nil {
		return manifest.Entry{}, err
	}
	if err := i.fs.Rename(t.TempPath, final); err != nil {
		return manifest.Entry{}, fmt.Errorf("install %s: %w", final, err)
	}
	if err := i.fs.SyncDir(path.Dir(final)); err != nil {
		return manifest.Entry{}, fmt.Errorf("flush directory of %s: %w", final, err)
	}

	entry := manifest.Entry{
		Path: rel, Kind: manifest.KindFile, Size: t.Size, ModTime: mtime,
		Mode: uint32(mode), Hash: t.Hash, SyncedHash: t.Hash,
	}
	return i.described(final, entry), nil
}

// sync flushes a partial file to disk.
func (i *Installer) sync(temp string) error {
	f, err := i.fs.OpenTemp(temp)
	if err != nil {
		return fmt.Errorf("open %s: %w", temp, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", temp, err)
	}
	return nil
}

// MkDir creates a directory described by a peer's entry.
func (i *Installer) MkDir(_ context.Context, root string, e manifest.Entry) (manifest.Entry, error) {
	full := path.Join(root, e.Path)
	mode := fs.FileMode(e.Mode).Perm()
	if mode == 0 {
		mode = defaultDirMode
	}
	if err := i.fs.MkdirAll(full, mode); err != nil {
		return manifest.Entry{}, fmt.Errorf("create directory %s: %w", full, err)
	}
	if err := i.fs.Chmod(full, mode); err != nil {
		return manifest.Entry{}, fmt.Errorf("set mode of %s: %w", full, err)
	}

	entry := manifest.Entry{Path: e.Path, Kind: manifest.KindDir, Mode: uint32(mode), ModTime: e.ModTime}
	return i.described(full, entry), nil
}

// RemoveReplaced deletes the local entry of a path whose kind changed on the
// peer, so the new kind can be created in its place. It is deliberately
// careful:
//
//   - a file is only removed when it still is the version the plan was made
//     against, so a change made since then is never lost;
//   - a directory is only removed when it is empty, which it is once the
//     deletions of the same plan have been applied to its children. One that
//     still holds entries the peer does not know about is kept.
//
// A path that is already gone is not an error.
func (i *Installer) RemoveReplaced(ctx context.Context, root, rel string, local *manifest.Entry) error {
	if err := i.CheckReplaceable(ctx, root, rel, local); err != nil {
		return err
	}
	full := path.Join(root, rel)
	if err := i.fs.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", full, err)
	}
	return nil
}

// CheckReplaceable is the read-only half of RemoveReplaced: it reports whether
// the local entry may give way, without touching anything. A file that must be
// downloaded first asks before the download rather than after it.
func (i *Installer) CheckReplaceable(ctx context.Context, root, rel string, local *manifest.Entry) error {
	full := path.Join(root, rel)
	info, err := i.fs.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", full, err)
	}

	switch {
	case info.IsDir():
		empty, err := i.dirEmpty(ctx, full)
		if err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("%s: %w", full, ErrDirNotEmpty)
		}
	case !info.IsSymlink():
		if local == nil || local.Kind != manifest.KindFile ||
			info.Size != local.Size || info.ModTime.UnixNano() != local.ModTime.UnixNano() {
			return fmt.Errorf("%s: %w", full, ErrChangedUnderneath)
		}
	}
	return nil
}

// Chmod sets the permission bits a peer announced on a local file whose
// content already matches. It returns the entry that now describes the file.
func (i *Installer) Chmod(_ context.Context, root string, e manifest.Entry) (manifest.Entry, error) {
	full := path.Join(root, e.Path)
	info, err := i.fs.Lstat(full)
	if err != nil {
		return manifest.Entry{}, fmt.Errorf("stat %s: %w", full, err)
	}
	if info.IsDir() || info.IsSymlink() {
		return manifest.Entry{}, fmt.Errorf("%s is no longer a file: %w", full, ErrChangedUnderneath)
	}

	mode := fs.FileMode(e.Mode).Perm()
	if err := i.fs.Chmod(full, mode); err != nil {
		return manifest.Entry{}, fmt.Errorf("set mode of %s: %w", full, err)
	}

	entry := manifest.Entry{
		Path: e.Path, Kind: manifest.KindFile, Size: info.Size, ModTime: info.ModTime,
		Mode: uint32(mode), Hash: e.Hash, SyncedHash: e.Hash,
	}
	return i.described(full, entry), nil
}

// dirEmpty reports whether a directory holds no entries at all.
func (i *Installer) dirEmpty(ctx context.Context, full string) (bool, error) {
	empty := true
	err := i.fs.Walk(ctx, full, func(rel string, _ ports.FileInfo) error {
		if rel == "" || rel == "." {
			return nil
		}
		empty = false
		return errStopWalk
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return false, fmt.Errorf("read %s: %w", full, err)
	}
	return empty, nil
}

// errStopWalk ends a walk that has seen enough.
var errStopWalk = errors.New("stop walk")

// Symlink creates a symbolic link described by a peer's entry, replacing a
// link that points somewhere else.
func (i *Installer) Symlink(_ context.Context, root string, e manifest.Entry) (manifest.Entry, error) {
	full := path.Join(root, e.Path)
	if info, err := i.fs.Lstat(full); err == nil {
		if !info.IsSymlink() {
			return manifest.Entry{}, fmt.Errorf("%s is not a symlink: %w", full, ErrChangedUnderneath)
		}
		if err := i.fs.Remove(full); err != nil {
			return manifest.Entry{}, fmt.Errorf("replace symlink %s: %w", full, err)
		}
	}
	if err := i.fs.MkdirAll(path.Dir(full), defaultDirMode); err != nil {
		return manifest.Entry{}, fmt.Errorf("create parent of %s: %w", full, err)
	}
	if err := i.fs.Symlink(e.LinkTarget, full); err != nil {
		return manifest.Entry{}, fmt.Errorf("create symlink %s: %w", full, err)
	}

	entry := manifest.Entry{
		Path: e.Path, Kind: manifest.KindSymlink, LinkTarget: e.LinkTarget,
		Mode: e.Mode, ModTime: e.ModTime,
	}
	return i.described(full, entry), nil
}

// Delete removes a path a peer deleted. A path that is already gone is not an
// error, and a directory that still holds files is left in place: its contents
// are deleted first, and the next cycle removes the directory.
func (i *Installer) Delete(_ context.Context, root, rel string) error {
	full := path.Join(root, rel)
	err := i.fs.Remove(full)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("delete %s: %w", full, err)
}

// checkTarget refuses to overwrite a target that is not the version the plan
// was made against.
func (i *Installer) checkTarget(final string, expect *manifest.Entry) error {
	info, err := i.fs.Lstat(final)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if expect != nil && !expect.Deleted {
			return fmt.Errorf("%s disappeared: %w", final, ErrChangedUnderneath)
		}
		return nil
	case err != nil:
		return fmt.Errorf("stat %s: %w", final, err)
	}
	if expect == nil || expect.Deleted {
		return fmt.Errorf("%s appeared while it was downloaded: %w", final, ErrChangedUnderneath)
	}
	if info.Size != expect.Size || info.ModTime.UnixNano() != expect.ModTime.UnixNano() {
		return fmt.Errorf("%s: %w", final, ErrChangedUnderneath)
	}
	return nil
}

// described fills in what only the filesystem knows about an installed entry,
// so the recorded version matches exactly what the next scan will see.
func (i *Installer) described(full string, e manifest.Entry) manifest.Entry {
	info, err := i.fs.Lstat(full)
	if err != nil {
		return e
	}
	e.Inode = info.Inode
	e.ModTime = info.ModTime
	e.Mode = uint32(info.Mode.Perm())
	if e.Kind == manifest.KindFile {
		e.Size = info.Size
	}
	return e
}

// hashFile returns the hash of a file's contents.
func (i *Installer) hashFile(p string) (manifest.Hash, error) {
	rc, err := i.fs.Open(p)
	if err != nil {
		return manifest.Hash{}, fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = rc.Close() }()

	h, err := i.hasher.Sum(rc)
	if err != nil {
		return manifest.Hash{}, fmt.Errorf("hash %s: %w", p, err)
	}
	return h, nil
}
