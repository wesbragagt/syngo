package syncer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// root is the folder the installer tests write into.
const root = "/home/dest/docs"

// installFixture is an installer with a partial file ready to be installed.
type installFixture struct {
	fs        *fakes.FileSystem
	hasher    fakes.Hasher
	installer *syncer.Installer
	transfer  transfer.Transfer
}

// newInstallFixture prepares a finished download of rel holding data.
func newInstallFixture(t *testing.T, rel string, data []byte) *installFixture {
	t.Helper()
	fsys := fakes.NewFileSystem()
	hasher := fakes.NewHasher()
	installer := syncer.NewInstaller(fsys, hasher)

	fsys.AddDir(root, 0o755)
	temp := installer.TempDir(root) + "/download.part"
	fsys.AddFile(temp, data, 0o600)

	return &installFixture{
		fs: fsys, hasher: hasher, installer: installer,
		transfer: transfer.Transfer{
			ID: "t1", FolderID: "docs", Path: rel, Hash: hasher.SumBytes(data),
			Size: int64(len(data)), Mode: 0o640, TempPath: temp,
			MTimeNS: time.Date(2026, time.May, 1, 2, 3, 4, 0, time.UTC).UnixNano(),
		},
	}
}

func TestTempDir_IsInsideTheFolderRoot(t *testing.T) {
	installer := syncer.NewInstaller(fakes.NewFileSystem(), fakes.NewHasher())
	if got := installer.TempDir(root); got != root+"/.syncthis/tmp" {
		t.Fatalf("temp dir = %q, want it inside the folder so a rename stays on one filesystem", got)
	}
}

func TestInstall_RenamesIntoPlaceWithModeAndMtime(t *testing.T) {
	data := []byte("the file contents")
	f := newInstallFixture(t, "sub/dir/a.txt", data)

	entry, err := f.installer.Install(context.Background(), root, f.transfer, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	got, ok := f.fs.Content(root + "/sub/dir/a.txt")
	if !ok || string(got) != string(data) {
		t.Fatalf("installed %q, want %q", got, data)
	}
	if _, ok := f.fs.Content(f.transfer.TempPath); ok {
		t.Fatal("the partial file is still there, the install did not rename it")
	}
	if renames := f.fs.Renames(); len(renames) != 1 || renames[0][1] != root+"/sub/dir/a.txt" {
		t.Fatalf("renames = %v, want one rename onto the target", renames)
	}

	info, err := f.fs.Lstat(root + "/sub/dir/a.txt")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode.Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", info.Mode.Perm())
	}
	if info.ModTime.UnixNano() != f.transfer.MTimeNS {
		t.Fatalf("mtime = %s, want %s", info.ModTime, time.Unix(0, f.transfer.MTimeNS))
	}
	if entry.Hash != f.transfer.Hash {
		t.Fatalf("entry = %+v, want it to carry the installed hash", entry)
	}
	if entry.Size != int64(len(data)) || entry.Kind != manifest.KindFile {
		t.Fatalf("entry = %+v, want it to describe the installed file", entry)
	}
}

func TestInstall_WholeFileHashMismatchInstallsNothing(t *testing.T) {
	f := newInstallFixture(t, "a.txt", []byte("what arrived"))
	f.transfer.Hash = f.hasher.SumBytes([]byte("what was announced"))

	_, err := f.installer.Install(context.Background(), root, f.transfer, nil)
	if !errors.Is(err, syncer.ErrIntegrity) {
		t.Fatalf("error = %v, want ErrIntegrity", err)
	}
	if _, ok := f.fs.Content(root + "/a.txt"); ok {
		t.Fatal("a file that failed verification was installed")
	}
	if _, ok := f.fs.Content(f.transfer.TempPath); !ok {
		t.Fatal("the partial file was removed, it must be kept for inspection")
	}
}

func TestInstall_RefusesWhenTargetChangedUnderneath(t *testing.T) {
	ctx := context.Background()
	local := []byte("the user's own version")

	t.Run("target appeared", func(t *testing.T) {
		f := newInstallFixture(t, "a.txt", []byte("from the peer"))
		f.fs.AddFile(root+"/a.txt", local, 0o644)

		_, err := f.installer.Install(ctx, root, f.transfer, nil)
		if !errors.Is(err, syncer.ErrChangedUnderneath) {
			t.Fatalf("error = %v, want ErrChangedUnderneath", err)
		}
		if got, _ := f.fs.Content(root + "/a.txt"); string(got) != string(local) {
			t.Fatalf("local file = %q, want it untouched", got)
		}
	})

	t.Run("target moved on", func(t *testing.T) {
		f := newInstallFixture(t, "a.txt", []byte("from the peer"))
		f.fs.AddFile(root+"/a.txt", local, 0o644)
		info, err := f.fs.Lstat(root + "/a.txt")
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		// The plan was made against a shorter local version.
		expect := manifest.Entry{Path: "a.txt", Size: info.Size - 1, ModTime: info.ModTime}

		if _, err := f.installer.Install(ctx, root, f.transfer, &expect); !errors.Is(err, syncer.ErrChangedUnderneath) {
			t.Fatalf("error = %v, want ErrChangedUnderneath", err)
		}
	})

	t.Run("target disappeared", func(t *testing.T) {
		f := newInstallFixture(t, "a.txt", []byte("from the peer"))
		expect := manifest.Entry{Path: "a.txt", Size: 3, ModTime: time.Now()}

		if _, err := f.installer.Install(ctx, root, f.transfer, &expect); !errors.Is(err, syncer.ErrChangedUnderneath) {
			t.Fatalf("error = %v, want ErrChangedUnderneath", err)
		}
	})

	t.Run("target is the expected version", func(t *testing.T) {
		f := newInstallFixture(t, "a.txt", []byte("from the peer"))
		f.fs.AddFile(root+"/a.txt", local, 0o644)
		info, err := f.fs.Lstat(root + "/a.txt")
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		expect := manifest.Entry{Path: "a.txt", Size: info.Size, ModTime: info.ModTime}

		if _, err := f.installer.Install(ctx, root, f.transfer, &expect); err != nil {
			t.Fatalf("install: %v", err)
		}
		if got, _ := f.fs.Content(root + "/a.txt"); string(got) != "from the peer" {
			t.Fatalf("installed %q, want the peer's version", got)
		}
	})
}

func TestMkDirAndSymlinkAndDelete(t *testing.T) {
	ctx := context.Background()
	fsys := fakes.NewFileSystem()
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())
	fsys.AddDir(root, 0o755)

	dir, err := installer.MkDir(ctx, root, manifest.Entry{Path: "nested/empty", Kind: manifest.KindDir, Mode: 0o750})
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if dir.Mode != 0o750 || dir.Kind != manifest.KindDir {
		t.Fatalf("entry = %+v, want a 0750 directory", dir)
	}

	link, err := installer.Symlink(ctx, root, manifest.Entry{
		Path: "nested/link", Kind: manifest.KindSymlink, LinkTarget: "empty",
	})
	if err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if link.LinkTarget != "empty" {
		t.Fatalf("entry = %+v, want it to point at empty", link)
	}

	// A link that points somewhere else is replaced, not left behind.
	if _, err := installer.Symlink(ctx, root, manifest.Entry{
		Path: "nested/link", Kind: manifest.KindSymlink, LinkTarget: "elsewhere",
	}); err != nil {
		t.Fatalf("replace symlink: %v", err)
	}
	target, err := fsys.ReadLink(root + "/nested/link")
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "elsewhere" {
		t.Fatalf("symlink points at %q, want %q", target, "elsewhere")
	}

	if err := installer.Delete(ctx, root, "nested/link"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := fsys.Lstat(root + "/nested/link"); err == nil {
		t.Fatal("the symlink is still there")
	}
	// Deleting what is already gone is not a failure.
	if err := installer.Delete(ctx, root, "nested/link"); err != nil {
		t.Fatalf("delete of a missing path: %v", err)
	}
}

func TestRemoveReplaced_FileMakesWayForADirectory(t *testing.T) {
	ctx := context.Background()
	fsys := fakes.NewFileSystem()
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())
	fsys.AddDir(root, 0o755)
	fsys.AddFile(root+"/f", []byte("the old file"), 0o644)

	info, err := fsys.Lstat(root + "/f")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	local := manifest.Entry{Path: "f", Kind: manifest.KindFile, Size: info.Size, ModTime: info.ModTime}

	if err := installer.RemoveReplaced(ctx, root, "f", &local); err != nil {
		t.Fatalf("remove replaced: %v", err)
	}
	if _, ok := fsys.Content(root + "/f"); ok {
		t.Fatal("the file is still there, the directory cannot be created")
	}
	if _, err := installer.MkDir(ctx, root, manifest.Entry{Path: "f", Kind: manifest.KindDir, Mode: 0o755}); err != nil {
		t.Fatalf("mkdir over the removed file: %v", err)
	}
}

func TestRemoveReplaced_KeepsALocallyChangedFile(t *testing.T) {
	fsys := fakes.NewFileSystem()
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())
	fsys.AddDir(root, 0o755)
	fsys.AddFile(root+"/f", []byte("what the user wrote"), 0o644)

	info, err := fsys.Lstat(root + "/f")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	// The plan was made against a shorter version of the file.
	local := manifest.Entry{Path: "f", Kind: manifest.KindFile, Size: info.Size - 1, ModTime: info.ModTime}

	err = installer.RemoveReplaced(context.Background(), root, "f", &local)
	if !errors.Is(err, syncer.ErrChangedUnderneath) {
		t.Fatalf("error = %v, want ErrChangedUnderneath", err)
	}
	if _, ok := fsys.Content(root + "/f"); !ok {
		t.Fatal("a file that changed since the plan was removed")
	}
}

func TestRemoveReplaced_Directories(t *testing.T) {
	ctx := context.Background()
	fsys := fakes.NewFileSystem()
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())
	fsys.AddDir(root, 0o755)
	fsys.AddDir(root+"/empty", 0o755)
	fsys.AddDir(root+"/full", 0o755)
	fsys.AddFile(root+"/full/keep.txt", []byte("only here"), 0o644)
	local := manifest.Entry{Kind: manifest.KindDir}

	if err := installer.RemoveReplaced(ctx, root, "empty", &local); err != nil {
		t.Fatalf("remove an empty directory: %v", err)
	}
	if _, err := fsys.Lstat(root + "/empty"); err == nil {
		t.Fatal("the empty directory is still there")
	}

	err := installer.RemoveReplaced(ctx, root, "full", &local)
	if !errors.Is(err, syncer.ErrDirNotEmpty) {
		t.Fatalf("error = %v, want ErrDirNotEmpty", err)
	}
	if _, ok := fsys.Content(root + "/full/keep.txt"); !ok {
		t.Fatal("a file only this side has was removed with its directory")
	}
	// The read-only check answers the same thing without touching anything.
	if err := installer.CheckReplaceable(ctx, root, "full", &local); !errors.Is(err, syncer.ErrDirNotEmpty) {
		t.Fatalf("check = %v, want ErrDirNotEmpty", err)
	}
	if _, err := fsys.Lstat(root + "/full"); err != nil {
		t.Fatalf("the directory was removed by a check: %v", err)
	}
}

func TestRemoveReplaced_MissingPathIsNotAFailure(t *testing.T) {
	fsys := fakes.NewFileSystem()
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())
	fsys.AddDir(root, 0o755)

	if err := installer.RemoveReplaced(context.Background(), root, "gone", nil); err != nil {
		t.Fatalf("remove a path that is already gone: %v", err)
	}
}

func TestChmod_SetsThePeersModeWithoutTouchingContent(t *testing.T) {
	fsys := fakes.NewFileSystem()
	hasher := fakes.NewHasher()
	installer := syncer.NewInstaller(fsys, hasher)
	data := []byte("unchanged content")
	fsys.AddDir(root, 0o755)
	fsys.AddFile(root+"/a.txt", data, 0o644)

	remote := manifest.Entry{
		Path: "a.txt", Kind: manifest.KindFile, Mode: 0o700, Hash: hasher.SumBytes(data),
	}
	entry, err := installer.Chmod(context.Background(), root, remote)
	if err != nil {
		t.Fatalf("chmod: %v", err)
	}

	info, err := fsys.Lstat(root + "/a.txt")
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode.Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", info.Mode.Perm())
	}
	if got, _ := fsys.Content(root + "/a.txt"); string(got) != string(data) {
		t.Fatalf("content = %q, want it untouched", got)
	}
	if entry.Mode != 0o700 || entry.Hash != remote.Hash {
		t.Fatalf("entry = %+v, want the new mode and the unchanged hash", entry)
	}
	if len(fsys.Renames()) != 0 {
		t.Fatalf("renames = %v, want a mode change to move nothing", fsys.Renames())
	}
}

func TestChmod_RefusesWhenThePathIsNoLongerAFile(t *testing.T) {
	fsys := fakes.NewFileSystem()
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())
	fsys.AddDir(root, 0o755)
	fsys.AddDir(root+"/a.txt", 0o755)

	_, err := installer.Chmod(context.Background(), root, manifest.Entry{Path: "a.txt", Mode: 0o700})
	if !errors.Is(err, syncer.ErrChangedUnderneath) {
		t.Fatalf("error = %v, want ErrChangedUnderneath", err)
	}
}
