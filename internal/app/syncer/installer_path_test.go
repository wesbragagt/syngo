package syncer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// hostilePaths are the paths a peer must never get the installer to write to.
var hostilePaths = []string{
	"..",
	"../escape.txt",
	"../../escape.txt",
	"a/../../escape.txt",
	"/etc/passwd",
	"",
	".",
	"./a",
	".syncthis/tmp/x",
	".syncthisignore",
	"a\x00b",
}

func TestInstall_RefusesAPathOutsideTheFolder(t *testing.T) {
	for _, rel := range hostilePaths {
		t.Run(rel, func(t *testing.T) {
			f := newInstallFixture(t, rel, []byte("hostile"))

			_, err := f.installer.Install(context.Background(), root, f.transfer, nil)
			if !errors.Is(err, syncer.ErrUnsafePath) {
				t.Fatalf("install %q = %v, want ErrUnsafePath", rel, err)
			}
			assertNothingOutside(t, f.fs)
		})
	}
}

func TestDelete_RefusesAPathOutsideTheFolder(t *testing.T) {
	fsys := fakes.NewFileSystem()
	fsys.AddDir(root, 0o755)
	fsys.AddFile("/home/dest/victim.txt", []byte("keep me"), 0o644)
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())

	// A peer tombstone for "../victim.txt" is the delete half of the same
	// traversal: without the guard it removes a file outside the folder.
	if err := installer.Delete(context.Background(), root, "../victim.txt"); !errors.Is(err, syncer.ErrUnsafePath) {
		t.Fatalf("delete = %v, want ErrUnsafePath", err)
	}
	if _, ok := fsys.Content("/home/dest/victim.txt"); !ok {
		t.Fatal("the file outside the folder was deleted")
	}
}

func TestMkDirAndChmod_RefuseAPathOutsideTheFolder(t *testing.T) {
	fsys := fakes.NewFileSystem()
	fsys.AddDir(root, 0o755)
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())
	ctx := context.Background()

	if _, err := installer.MkDir(ctx, root, manifest.Entry{
		Path: "../escape", Kind: manifest.KindDir, Mode: 0o755,
	}); !errors.Is(err, syncer.ErrUnsafePath) {
		t.Fatalf("mkdir = %v, want ErrUnsafePath", err)
	}
	if _, err := installer.Chmod(ctx, root, manifest.Entry{
		Path: "../escape.txt", Kind: manifest.KindFile, Mode: 0o777,
	}, nil); !errors.Is(err, syncer.ErrUnsafePath) {
		t.Fatalf("chmod = %v, want ErrUnsafePath", err)
	}
	if err := installer.RemoveReplaced(ctx, root, "../escape.txt", nil); !errors.Is(err, syncer.ErrUnsafePath) {
		t.Fatalf("remove replaced = %v, want ErrUnsafePath", err)
	}
	assertNothingOutside(t, fsys)
}

func TestSymlink_RefusesATargetThatLeavesTheFolder(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		target string
	}{
		{name: "absolute target", path: "link", target: "/etc"},
		{name: "target climbing out", path: "link", target: "../.."},
		{name: "target climbing out of a subdirectory", path: "a/link", target: "../../outside"},
		{name: "target reaching the state directory", path: "link", target: ".syncthis"},
		{name: "path outside the folder", path: "../link", target: "file.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fakes.NewFileSystem()
			fsys.AddDir(root, 0o755)
			installer := syncer.NewInstaller(fsys, fakes.NewHasher())

			_, err := installer.Symlink(context.Background(), root, manifest.Entry{
				Path: tc.path, Kind: manifest.KindSymlink, LinkTarget: tc.target,
			})
			if !errors.Is(err, syncer.ErrUnsafePath) {
				t.Fatalf("symlink = %v, want ErrUnsafePath", err)
			}
			assertNothingOutside(t, fsys)
		})
	}
}

// TestInstall_RefusesToWriteThroughASymlinkedParent covers the second half of
// the symlink escape: the link itself is inside the folder, but a peer that
// got one installed before the target check existed must still not be able to
// write through it.
func TestInstall_RefusesToWriteThroughASymlinkedParent(t *testing.T) {
	f := newInstallFixture(t, "a/passwd", []byte("hostile"))
	f.fs.AddDir("/outside", 0o755)
	f.fs.AddSymlink(root+"/a", "/outside")

	_, err := f.installer.Install(context.Background(), root, f.transfer, nil)
	if !errors.Is(err, syncer.ErrUnsafePath) {
		t.Fatalf("install = %v, want ErrUnsafePath", err)
	}
	if _, ok := f.fs.Content("/outside/passwd"); ok {
		t.Fatal("a file was written through the symlinked parent")
	}
}

func TestMkDir_RefusesASymlinkedParent(t *testing.T) {
	fsys := fakes.NewFileSystem()
	fsys.AddDir(root, 0o755)
	fsys.AddDir("/outside", 0o755)
	fsys.AddSymlink(root+"/a", "/outside")
	installer := syncer.NewInstaller(fsys, fakes.NewHasher())

	_, err := installer.MkDir(context.Background(), root, manifest.Entry{
		Path: "a/sub", Kind: manifest.KindDir, Mode: 0o755,
	})
	if !errors.Is(err, syncer.ErrUnsafePath) {
		t.Fatalf("mkdir = %v, want ErrUnsafePath", err)
	}
	if _, err := fsys.Lstat("/outside/sub"); err == nil {
		t.Fatal("a directory was created through the symlinked parent")
	}
}

// assertNothingOutside fails when anything exists beside the folder root.
func assertNothingOutside(t *testing.T, fsys *fakes.FileSystem) {
	t.Helper()
	for _, p := range fsys.Paths() {
		if p == "/" || strings.HasPrefix(p, root+"/") || p == root {
			continue
		}
		if p == "/home" || p == "/home/dest" || p == "/outside" {
			// The ancestors of the root, and the directory a test
			// put there itself, are not what the guard is about.
			continue
		}
		t.Fatalf("%q exists outside the folder root", p)
	}
}
