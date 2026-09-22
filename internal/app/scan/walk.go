package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/ignore"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

const (
	// ReservedDir is the per-folder state directory. It is never part of a
	// manifest, whatever the ignore rules say.
	ReservedDir = manifest.ReservedDir
	// IgnoreFile is the per-folder ignore file, read from the folder root.
	IgnoreFile = manifest.IgnoreFile
	// ignoreFileMax bounds how much of the ignore file is read.
	ignoreFileMax = 1 << 20
	// cancelCheckEvery is how many visited entries pass between two context
	// cancellation checks.
	cancelCheckEvery = 100
)

// LoadIgnore builds the matcher for a folder: the always-ignored paths first,
// then the configured patterns, then the lines of <root>/.syncthisignore. Later
// rules win, so the ignore file can re-include what a configured pattern
// excluded. A missing ignore file is not an error.
func LoadIgnore(ctx context.Context, fsys ports.FileSystem, f folder.Folder) (*ignore.Matcher, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	base, err := ignore.Parse([]string{ReservedDir + "/", IgnoreFile})
	if err != nil {
		return nil, fmt.Errorf("build base ignore rules: %w", err)
	}
	configured, err := ignore.Parse(f.IgnorePatterns)
	if err != nil {
		return nil, fmt.Errorf("folder %q ignore patterns: %w", f.ID, err)
	}

	lines, err := readIgnoreFile(fsys, path.Join(f.Path, IgnoreFile))
	if err != nil {
		return nil, err
	}
	fromFile, err := ignore.Parse(lines)
	if err != nil {
		return nil, fmt.Errorf("folder %q %s: %w", f.ID, IgnoreFile, err)
	}
	return ignore.Combine(base, configured, fromFile), nil
}

// readIgnoreFile returns the lines of an ignore file, or nil if it is absent.
func readIgnoreFile(fsys ports.FileSystem, p string) ([]string, error) {
	rc, err := fsys.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, ignoreFileMax))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return strings.Split(string(data), "\n"), nil
}

// walker builds the current manifest of a folder from what is on disk.
type walker struct {
	scanner *Scanner
	folder  folder.Folder
	opts    Options
	matcher *ignore.Matcher
	old     manifest.Manifest
	cur     manifest.Manifest
	result  *Result
	visited int
}

// collect walks the subtree sub of the folder root, adding every entry that
// survives the ignore rules and policies to w.cur. A subtree that no longer
// exists contributes nothing, which makes its stored entries deletions.
func (w *walker) collect(ctx context.Context, sub string) error {
	root := w.folder.Path
	if sub != "" {
		root = path.Join(root, sub)
	}

	err := w.scanner.fs.Walk(ctx, root, func(rel string, info ports.FileInfo) error {
		return w.visit(ctx, path.Join(sub, rel), info)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan folder %q: %w", w.folder.ID, err)
	}
	return nil
}

// visit classifies one filesystem object and records it when it belongs to the
// folder. It returns ports.SkipDir to prune a directory.
func (w *walker) visit(ctx context.Context, rel string, info ports.FileInfo) error {
	w.visited++
	if w.visited%cancelCheckEvery == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if rel == "" || rel == "." {
		return nil
	}

	if skip, err := w.skipped(rel, info); skip {
		w.result.Ignored++
		return err
	}

	switch {
	case info.IsSymlink():
		return w.addSymlink(rel, info)
	case info.IsDir():
		if w.folder.KeepEmptyDirs {
			w.add(w.carrying(manifest.Entry{
				Path: rel, Kind: manifest.KindDir, ModTime: info.ModTime,
				Mode: uint32(info.Mode.Perm()), Inode: info.Inode,
			}))
		}
		return nil
	case info.Mode.IsRegular():
		return w.addFile(rel, info)
	default:
		// Sockets, devices and fifos have no content to synchronize.
		w.result.Ignored++
		return nil
	}
}

// skipped reports whether rel is excluded, together with the value the walk
// function must return to prune it.
func (w *walker) skipped(rel string, info ports.FileInfo) (bool, error) {
	prune := error(nil)
	if info.IsDir() {
		prune = ports.SkipDir
	}
	switch {
	case rel == ReservedDir || strings.HasPrefix(rel, ReservedDir+"/"):
		return true, prune
	case !w.folder.IncludeHidden && strings.HasPrefix(path.Base(rel), "."):
		return true, prune
	case w.matcher.Match(rel, info.IsDir()):
		return true, prune
	default:
		return false, nil
	}
}

// addSymlink records a symlink, or ignores it when the policy says to.
func (w *walker) addSymlink(rel string, info ports.FileInfo) error {
	if w.folder.Symlinks != folder.SymlinkStore {
		w.result.Ignored++
		return nil
	}
	target, err := w.scanner.fs.ReadLink(path.Join(w.folder.Path, rel))
	if err != nil {
		w.skip(rel, err)
		return nil
	}
	w.add(w.carrying(manifest.Entry{
		Path: rel, Kind: manifest.KindSymlink, ModTime: info.ModTime,
		Mode: uint32(info.Mode.Perm()), LinkTarget: target, Inode: info.Inode,
	}))
	return nil
}

// addFile records a file, hashing it unless the stored entry still matches
// what is on disk.
func (w *walker) addFile(rel string, info ports.FileInfo) error {
	e := manifest.Entry{
		Path: rel, Kind: manifest.KindFile, Size: info.Size, ModTime: info.ModTime,
		Mode: uint32(info.Mode.Perm()), Inode: info.Inode,
	}

	if prev, ok := w.old.Entries[rel]; ok && unchanged(prev, info) {
		e.Hash = prev.Hash
		e.SyncedMode = prev.SyncedMode
		w.add(w.carrying(e))
		return nil
	}

	hash, err := w.hash(path.Join(w.folder.Path, rel))
	if err != nil {
		w.skip(rel, err)
		return nil
	}
	e.Hash = hash
	if prev, ok := w.old.Entries[rel]; ok {
		e.SyncedMode = prev.SyncedMode
	}
	w.result.Hashed++
	w.result.Bytes += info.Size
	w.add(w.carrying(e))
	return nil
}

// hash reads a file and returns the hash of its contents.
func (w *walker) hash(p string) (manifest.Hash, error) {
	rc, err := w.scanner.fs.Open(p)
	if err != nil {
		return manifest.Hash{}, err
	}
	defer func() { _ = rc.Close() }()

	h, err := w.scanner.hasher.Sum(rc)
	if err != nil {
		return manifest.Hash{}, fmt.Errorf("hash %s: %w", p, err)
	}
	return h, nil
}

// skip records an entry the scan could not read. The stored version, if there
// is one, is carried over unchanged so an unreadable file is not classified as
// a deletion, and the folder keeps the hash it already knows.
func (w *walker) skip(rel string, err error) {
	w.result.Skipped++
	if len(w.result.Warnings) < MaxWarnings {
		w.result.Warnings = append(w.result.Warnings, fmt.Sprintf("%s: %v", rel, err))
	}
	w.scanner.log.Debug("skipping unreadable entry", "folder", w.folder.ID, "path", rel, "error", err)
	if prev, ok := w.old.Entries[rel]; ok && !prev.Deleted {
		w.cur.Add(prev)
	}
}

// carrying gives an entry the history the stored one has: the version vector
// and the author of the content. A change raises the vector when it is
// written; what the walk builds is the manifest as it is now, in which an
// untouched path still carries what it always carried.
func (w *walker) carrying(e manifest.Entry) manifest.Entry {
	if prev, ok := w.old.Entries[e.Path]; ok {
		e.Version = prev.Version
		e.ModifiedBy = prev.ModifiedBy
	}
	return e
}

// add records an entry and reports progress.
func (w *walker) add(e manifest.Entry) {
	w.cur.Add(e)
	w.result.Scanned++
	if n := w.opts.ProgressEvery; n > 0 && w.result.Scanned%n == 0 {
		w.scanner.publish("scan.progress", map[string]any{
			"folder_id": string(w.folder.ID),
			"scanned":   w.result.Scanned,
			"hashed":    w.result.Hashed,
		})
	}
}

// unchanged reports whether a stored file entry still describes what lstat
// found, in which case its hash can be reused.
func unchanged(prev manifest.Entry, info ports.FileInfo) bool {
	return !prev.Deleted &&
		prev.Kind == manifest.KindFile &&
		!prev.Hash.IsZero() &&
		prev.Size == info.Size &&
		prev.ModTime.UnixNano() == info.ModTime.UnixNano() &&
		prev.Inode == info.Inode
}
