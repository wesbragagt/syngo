// Package conflicts is what `syncthis conflict list`, `show`, `resolve` and
// `export` do: it reads the conflict records the puller wrote and carries out
// the one decision only the user can make, which version of a diverged path to
// keep.
//
// Nothing here ever deletes a version without being told which one to keep, and
// the version that is kept is put in place with a rename, so a failure halfway
// leaves both versions on disk.
package conflicts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Errors reported to the user.
var (
	// ErrVersionGone reports that neither file holds the version the user
	// asked to keep any more, because something changed them since the
	// conflict was recorded. Nothing is touched.
	ErrVersionGone = errors.New("the version asked for is no longer on disk")
	// ErrUnknownKeep reports a --keep value that is not local, remote or both.
	ErrUnknownKeep = errors.New("unknown version to keep")
	// ErrChanged reports that a file a resolution would delete or
	// overwrite no longer holds either of the two versions the conflict
	// recorded, so carrying the resolution out would throw away content
	// nobody decided about. Nothing is touched.
	ErrChanged = errors.New("has changed since the conflict was recorded")
	// ErrAlreadyResolved reports a conflict that was already decided. It
	// is not reopened: resolving it again would act on files a later
	// conflict may own.
	ErrAlreadyResolved = errors.New("is already resolved")
)

// Keep names the version a resolution keeps.
type Keep string

// The versions a conflict can be resolved to.
const (
	// KeepLocal keeps the version this device had when the conflict was
	// detected, whichever of the two files holds it now.
	KeepLocal Keep = "local"
	// KeepRemote keeps the version the peer had, which includes the peer
	// having deleted the path.
	KeepRemote Keep = "remote"
	// KeepBoth leaves both files alone and only closes the record.
	KeepBoth Keep = "both"
)

// ParseKeep validates a --keep value.
func ParseKeep(s string) (Keep, error) {
	switch Keep(s) {
	case KeepLocal, KeepRemote, KeepBoth:
		return Keep(s), nil
	default:
		return "", fmt.Errorf("%w %q, want local, remote or both", ErrUnknownKeep, s)
	}
}

// Store is the persistence a conflict service needs.
type Store interface {
	ports.ConflictStore
	ports.FolderStore
}

// File describes one of the two versions of a conflict as it is on disk.
type File struct {
	Path    string
	Size    int64
	ModTime time.Time
	// Mode is the permission bits the file has, which an export keeps.
	Mode fs.FileMode
	// Hash is what the file holds now, which may differ from the hash the
	// conflict recorded if it was edited since.
	Hash manifest.Hash
}

// View is one conflict as reported to the user. File and Copy are filled in by
// Show, by Resolve and by Export; a listing leaves them out, because it would
// be a table of hashes nobody asked for, but it does hash both files to say
// whether the conflict has gone stale.
type View struct {
	ID           string
	FolderID     folder.ID
	FolderPath   string
	Path         string
	ConflictPath string
	Reason       string
	LocalHash    manifest.Hash
	RemoteHash   manifest.Hash
	PeerID       device.ID
	DetectedAt   time.Time
	Resolved     bool
	// File describes what is at Path and Copy what is at ConflictPath.
	// Either is nil when there is no file there. Which of them holds the
	// local and which the remote version is what their hashes say: a
	// resolution goes by content, not by position.
	File *File
	Copy *File
	// Stale reports that neither version can be kept any more, because
	// what is on disk moved on since the conflict was recorded. Such a
	// conflict is closed with --keep both, after looking at the files.
	Stale bool
}

// Resolution is what a resolve did.
type Resolution struct {
	View View
	Kept Keep
	// KeptPath is the path holding the kept version afterwards, empty when
	// the kept version is the peer's deletion.
	KeptPath string
	// RemovedPath is the file that was deleted, empty when none was.
	RemovedPath string
	// MovedFrom is the copy that was renamed onto Path, empty when the kept
	// version was already there.
	MovedFrom string
}

// Export is where the two versions of a conflict were written.
type Export struct {
	View View
	Dir  string
	// Files are the copies written, relative to Dir.
	Files []string
	// Sidecar is the JSON description written beside them, relative to Dir.
	Sidecar string
}

// Service reads and resolves conflicts.
type Service struct {
	store  Store
	fs     ports.FileSystem
	hasher ports.Hasher
}

// New returns a service working on store and fsys.
func New(store Store, fsys ports.FileSystem, hasher ports.Hasher) *Service {
	return &Service{store: store, fs: fsys, hasher: hasher}
}

// List returns the conflicts of a folder, or of every folder when id is empty.
// Resolved ones are left out unless includeResolved is set.
func (s *Service) List(ctx context.Context, id folder.ID, includeResolved bool) ([]View, error) {
	if id != "" {
		// A folder that does not exist is reported as such rather than as
		// an empty list.
		if _, err := s.store.GetFolder(ctx, id); err != nil {
			return nil, err
		}
	}
	records, err := s.store.ListConflicts(ctx, id, includeResolved)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(records))
	for _, rec := range records {
		f, err := s.store.GetFolder(ctx, rec.FolderID)
		if errors.Is(err, ports.ErrNotFound) {
			// The folder the record belongs to is gone. Its path is
			// what every path in the view is relative to, so with
			// the zero value the listing would describe whatever
			// sits at those paths from the working directory. The
			// record is left out instead.
			continue
		}
		if err != nil {
			return nil, err
		}
		// A listing says whether a conflict went stale, which takes
		// hashing both files, but it does not report the files
		// themselves: that is what show is for.
		view := s.described(viewOf(rec, f.Path))
		view.File, view.Copy = nil, nil
		out = append(out, view)
	}
	return out, nil
}

// Show returns one conflict together with what is on disk for both versions.
func (s *Service) Show(ctx context.Context, id string) (View, error) {
	view, err := s.load(ctx, id)
	if err != nil {
		return View{}, err
	}
	return s.described(view), nil
}

// Resolve carries out the decision the user made. Keeping both versions only
// closes the record; keeping one of them puts it at the conflict's path and
// removes the other file, unless the version kept is the peer's deletion, in
// which case the local file goes.
func (s *Service) Resolve(ctx context.Context, id string, keep Keep) (Resolution, error) {
	view, err := s.load(ctx, id)
	if err != nil {
		return Resolution{}, err
	}
	if view.Resolved {
		return Resolution{}, fmt.Errorf("conflict %q %w", id, ErrAlreadyResolved)
	}
	view = s.described(view)
	result := Resolution{View: view, Kept: keep}

	switch keep {
	case KeepBoth:
		result.KeptPath = view.Path
	case KeepLocal, KeepRemote:
		result, err = s.keepOne(view, keep)
		if err != nil {
			return Resolution{}, err
		}
	default:
		return Resolution{}, fmt.Errorf("%w %q", ErrUnknownKeep, keep)
	}

	if err := s.store.SetConflictResolved(ctx, id, true); err != nil {
		return Resolution{}, err
	}
	result.View.Resolved = true
	return result, nil
}

// keepOne puts the version the user chose at the conflict's path and removes
// the file holding the other one.
func (s *Service) keepOne(view View, keep Keep) (Resolution, error) {
	result := Resolution{View: view, Kept: keep}
	want := view.LocalHash
	if keep == KeepRemote {
		want = view.RemoteHash
	}
	// Nothing is touched before it is clear that every file this would
	// delete or overwrite still holds a version the conflict recorded.
	if err := view.check(want); err != nil {
		return Resolution{}, err
	}

	switch {
	// The peer deleted the path: keeping its version means removing the
	// local file, and there is no copy to put anywhere.
	case want.IsZero():
		if view.File == nil {
			return result, nil
		}
		if err := s.remove(view.FolderPath, view.Path); err != nil {
			return Resolution{}, err
		}
		result.RemovedPath = view.Path
	case view.File != nil && view.File.Hash == want:
		result.KeptPath = view.Path
		if view.Copy != nil {
			if err := s.remove(view.FolderPath, view.Copy.Path); err != nil {
				return Resolution{}, err
			}
			result.RemovedPath = view.Copy.Path
		}
	default:
		// The copy holds the version that wins. The rename replaces the
		// other version in one step, so the path never holds a
		// half-written file.
		if err := s.rename(view.FolderPath, view.Copy.Path, view.Path); err != nil {
			return Resolution{}, err
		}
		result.KeptPath = view.Path
		result.MovedFrom = view.Copy.Path
		result.RemovedPath = view.Copy.Path
	}
	return result, nil
}

// check reports whether keeping the version hashing to want can be carried
// out. It answers no when no file holds that version any more, and when a file
// the resolution would delete or overwrite holds something neither side had
// when the conflict was recorded: that content was never decided about, so it
// is not thrown away.
func (v View) check(want manifest.Hash) error {
	switch {
	case want.IsZero():
		// Following the peer's deletion removes whatever is at the path.
		if v.File != nil && !v.recorded(v.File) {
			return changedError(v.Path)
		}
		return nil
	case v.File != nil && v.File.Hash == want:
		// The copy is the file that goes.
		if v.Copy != nil && !v.recorded(v.Copy) {
			return changedError(v.Copy.Path)
		}
		return nil
	case v.Copy != nil && v.Copy.Hash == want:
		// The copy is renamed onto the path, over whatever is there.
		if v.File != nil && !v.recorded(v.File) {
			return changedError(v.Path)
		}
		return nil
	default:
		return fmt.Errorf("%s: %w", v.Path, ErrVersionGone)
	}
}

// recorded reports whether a file still holds one of the two versions the
// conflict recorded, which is what makes it safe to delete or overwrite.
func (v View) recorded(f *File) bool {
	return (!v.LocalHash.IsZero() && f.Hash == v.LocalHash) ||
		(!v.RemoteHash.IsZero() && f.Hash == v.RemoteHash)
}

// changedError reports a file that moved on since the conflict was recorded
// and says what can still be done about it.
func changedError(rel string) error {
	return fmt.Errorf("%s %w: keep both versions with --keep both, or resolve again after looking at the file",
		rel, ErrChanged)
}

// Export copies both versions of a conflict into dir, together with a JSON
// file describing which copy holds which version. Nothing in the folder is
// touched and the conflict stays open.
func (s *Service) Export(ctx context.Context, id, dir string) (Export, error) {
	view, err := s.load(ctx, id)
	if err != nil {
		return Export{}, err
	}
	view = s.described(view)
	if err := s.fs.MkdirAll(dir, 0o755); err != nil {
		// The hint names the directory dir goes in: that is the one
		// whose permissions stood in the way.
		return Export{}, exportError(path.Dir(dir), fmt.Errorf("create the export directory: %w", err))
	}

	out := Export{View: view, Dir: dir, Sidecar: view.ID + ".json"}
	sidecar := sidecarOf(view)
	for _, version := range []struct {
		file *File
		side string
	}{{view.File, "path"}, {view.Copy, "conflict"}} {
		if version.file == nil {
			continue
		}
		name := version.side + "-" + path.Base(version.file.Path)
		// The copy keeps the permission bits of the file it came from,
		// so an exported script stays runnable and a private file stays
		// private.
		if err := s.copy(path.Join(view.FolderPath, version.file.Path), path.Join(dir, name), version.file.Mode); err != nil {
			return Export{}, exportError(dir, err)
		}
		out.Files = append(out.Files, name)
		sidecar.Files = append(sidecar.Files, exportedFile{
			Name: name, From: version.file.Path, Hash: version.file.Hash.String(),
			Size: version.file.Size, Version: versionName(view, version.file.Hash),
		})
	}

	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return Export{}, fmt.Errorf("describe conflict %q: %w", id, err)
	}
	if err := s.write(path.Join(dir, out.Sidecar), data, 0o644); err != nil {
		return Export{}, exportError(dir, err)
	}
	return out, nil
}

// load reads a conflict and the folder it belongs to.
func (s *Service) load(ctx context.Context, id string) (View, error) {
	rec, err := s.store.GetConflict(ctx, id)
	if err != nil {
		return View{}, err
	}
	f, err := s.store.GetFolder(ctx, rec.FolderID)
	if err != nil {
		return View{}, err
	}
	return viewOf(rec, f.Path), nil
}

// described fills in what the two files hold now, and whether the conflict
// went stale because neither version can be kept any more.
func (s *Service) described(view View) View {
	view.File = s.describe(view.FolderPath, view.Path)
	if view.ConflictPath != "" {
		view.Copy = s.describe(view.FolderPath, view.ConflictPath)
	}
	view.Stale = view.check(view.LocalHash) != nil && view.check(view.RemoteHash) != nil
	return view
}

// Holder returns the file holding one of the two versions, or nil when no file
// holds it any more.
func (v View) Holder(hash manifest.Hash) *File {
	switch {
	case v.File != nil && v.File.Hash == hash:
		return v.File
	case v.Copy != nil && v.Copy.Hash == hash:
		return v.Copy
	default:
		return nil
	}
}

// describe reads the size, time and content hash of one version, or returns
// nil when there is no file at that path any more.
func (s *Service) describe(root, rel string) *File {
	full := path.Join(root, rel)
	info, err := s.fs.Lstat(full)
	if err != nil || info.IsDir() || info.IsSymlink() {
		return nil
	}
	file := &File{Path: rel, Size: info.Size, ModTime: info.ModTime, Mode: info.Mode.Perm()}
	rc, err := s.fs.Open(full)
	if err != nil {
		return file
	}
	defer func() { _ = rc.Close() }()
	if hash, err := s.hasher.Sum(rc); err == nil {
		file.Hash = hash
	}
	return file
}

// remove deletes one file of a folder.
func (s *Service) remove(root, rel string) error {
	full := path.Join(root, rel)
	if err := s.fs.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", full, err)
	}
	return nil
}

// rename moves one file of a folder onto another, replacing it atomically.
func (s *Service) rename(root, from, to string) error {
	full := path.Join(root, to)
	if err := s.fs.Rename(path.Join(root, from), full); err != nil {
		return fmt.Errorf("keep %s as %s: %w", from, to, err)
	}
	if err := s.fs.SyncDir(path.Dir(full)); err != nil {
		return fmt.Errorf("flush directory of %s: %w", full, err)
	}
	return nil
}

// copy writes the contents of one file to another path, with the permission
// bits mode.
func (s *Service) copy(from, to string, mode fs.FileMode) error {
	rc, err := s.fs.Open(from)
	if err != nil {
		return fmt.Errorf("open %s: %w", from, err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read %s: %w", from, err)
	}
	return s.write(to, data, mode)
}

// write creates a file holding data with the permission bits mode, replacing
// anything at that path. The bytes reach a temporary file first, so a failure
// never leaves half a file behind under the final name; the temporary file is
// created private, so the bits are set before it is visible under its name.
func (s *Service) write(to string, data []byte, mode fs.FileMode) error {
	temp, err := s.fs.CreateTemp(path.Dir(to), path.Base(to)+".*")
	if err != nil {
		return fmt.Errorf("create %s: %w", to, err)
	}
	name := temp.Name()
	if _, err := temp.WriteAt(data, 0); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("flush %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := s.fs.Chmod(name, mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", name, err)
	}
	if err := s.fs.Rename(name, to); err != nil {
		return fmt.Errorf("write %s: %w", to, err)
	}
	return nil
}

// exportError says what to do about an export the filesystem refused, which is
// most often a directory the user cannot write into.
func exportError(dir string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: check that %s can be written to", err, dir)
	}
	return err
}

// sidecar is the JSON document an export writes beside the two copies.
type sidecar struct {
	ID           string         `json:"id"`
	FolderID     string         `json:"folder_id"`
	FolderPath   string         `json:"folder_path"`
	Path         string         `json:"path"`
	ConflictPath string         `json:"conflict_path"`
	Reason       string         `json:"reason"`
	LocalHash    string         `json:"local_hash"`
	RemoteHash   string         `json:"remote_hash"`
	PeerID       string         `json:"peer_id"`
	DetectedAt   string         `json:"detected_at"`
	Resolved     bool           `json:"resolved"`
	Files        []exportedFile `json:"files"`
}

// exportedFile is one copy an export wrote.
type exportedFile struct {
	Name    string `json:"name"`
	From    string `json:"from"`
	Version string `json:"version"`
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
}

// sidecarOf describes a conflict without its copies.
func sidecarOf(view View) sidecar {
	return sidecar{
		ID: view.ID, FolderID: string(view.FolderID), FolderPath: view.FolderPath,
		Path: view.Path, ConflictPath: view.ConflictPath, Reason: view.Reason,
		LocalHash: view.LocalHash.String(), RemoteHash: view.RemoteHash.String(),
		PeerID: string(view.PeerID), DetectedAt: view.DetectedAt.UTC().Format(time.RFC3339),
		Resolved: view.Resolved, Files: []exportedFile{},
	}
}

// versionName says whose version a hash is, or "unknown" when the file holds
// something neither side had when the conflict was recorded.
func versionName(view View, hash manifest.Hash) string {
	switch hash {
	case view.LocalHash:
		return string(KeepLocal)
	case view.RemoteHash:
		return string(KeepRemote)
	default:
		return "unknown"
	}
}

// viewOf turns a record into a view.
func viewOf(rec ports.ConflictRecord, folderPath string) View {
	return View{
		ID: rec.ID, FolderID: rec.FolderID, FolderPath: folderPath, Path: rec.Path,
		ConflictPath: rec.ConflictPath, Reason: rec.Reason, LocalHash: rec.LocalHash,
		RemoteHash: rec.RemoteHash, PeerID: rec.PeerID, DetectedAt: rec.DetectedAt,
		Resolved: rec.Resolved,
	}
}
