package scan_test

import (
	"context"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// root is the folder path every test scans.
const root = "/docs"

// countingHasher counts the files a scan actually reads, and optionally runs a
// hook before each hash so a test can cancel mid-walk.
type countingHasher struct {
	inner  fakes.Hasher
	calls  int
	before func(n int)
}

// Sum hashes r, counting the call.
func (h *countingHasher) Sum(r io.Reader) (manifest.Hash, error) {
	h.calls++
	if h.before != nil {
		h.before(h.calls)
	}
	return h.inner.Sum(r)
}

// SumBytes hashes b.
func (h *countingHasher) SumBytes(b []byte) manifest.Hash { return h.inner.SumBytes(b) }

// env is a scanner wired to in-memory fakes.
type env struct {
	fs      *fakes.FileSystem
	store   *fakes.Store
	clock   *fakes.Clock
	hasher  *countingHasher
	events  *fakes.Events
	scanner *scan.Scanner
	folder  folder.Folder
}

// newEnv returns a scanner over an empty folder at /docs.
func newEnv(t *testing.T, customize ...func(*folder.Folder)) *env {
	t.Helper()

	f := folder.Folder{
		ID: "docs", Path: root, Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkSkip, KeepEmptyDirs: true,
	}
	for _, fn := range customize {
		fn(&f)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("folder config: %v", err)
	}

	e := &env{
		fs:     fakes.NewFileSystem(),
		store:  fakes.NewStore(),
		clock:  fakes.NewClock(time.Time{}),
		hasher: &countingHasher{},
		events: fakes.NewEvents(),
		folder: f,
	}
	e.fs.AddDir(root, 0o755)
	if err := e.store.SaveFolder(context.Background(), f); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	e.scanner = scan.New(e.fs, e.hasher, e.store, e.clock, fakes.NewIdentity("DEV1", "test"), e.events, nil)
	return e
}

// scan runs a scan and fails the test on error.
func (e *env) scan(t *testing.T, o scan.Options) scan.Result {
	t.Helper()
	res, err := e.scanner.Scan(context.Background(), e.folder, o)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res
}

// write creates or replaces a file, giving it a fresh inode like a rewrite
// through a new file would.
func (e *env) write(p, content string) {
	e.fs.AddFile(root+"/"+p, []byte(content), 0o644)
}

// overwrite rewrites a file in place, keeping its inode, and moves its
// modification time forward. This is what an in-place editor does.
func (e *env) overwrite(t *testing.T, p, content string) {
	t.Helper()
	full := root + "/" + p
	tf, err := e.fs.OpenTemp(full)
	if err != nil {
		t.Fatalf("open %s: %v", full, err)
	}
	if err := tf.Truncate(0); err != nil {
		t.Fatalf("truncate %s: %v", full, err)
	}
	if _, err := tf.WriteAt([]byte(content), 0); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	if err := tf.Close(); err != nil {
		t.Fatalf("close %s: %v", full, err)
	}
	info, err := e.fs.Lstat(full)
	if err != nil {
		t.Fatalf("lstat %s: %v", full, err)
	}
	if err := e.fs.Chtimes(full, info.ModTime.Add(time.Second)); err != nil {
		t.Fatalf("chtimes %s: %v", full, err)
	}
}

// remove deletes a path from the fake filesystem.
func (e *env) remove(t *testing.T, p string) {
	t.Helper()
	if err := e.fs.Remove(root + "/" + p); err != nil {
		t.Fatalf("remove %s: %v", p, err)
	}
}

// stored returns the live entry paths currently in the store.
func (e *env) stored(t *testing.T) []string {
	t.Helper()
	m, err := e.store.LoadManifest(context.Background(), e.folder.ID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	var out []string
	for _, entry := range m.Live() {
		out = append(out, entry.Path)
	}
	sort.Strings(out)
	return out
}

// summary renders the changes of a result as "type:path" strings, sorted.
func summary(r scan.Result) []string {
	out := make([]string, 0, len(r.Changes))
	for _, c := range r.Changes {
		s := c.Type.String() + ":" + c.Path
		if c.OldPath != "" {
			s += "<-" + c.OldPath
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// equal reports whether two string slices hold the same values in order.
func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScan_InitialCreatesAll(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write("sub/b.txt", "beta")

	res := e.scan(t, scan.Options{})

	want := []string{"create:a.txt", "create:sub", "create:sub/b.txt"}
	if got := summary(res); !equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if res.Hashed != 2 || res.Scanned != 3 {
		t.Fatalf("scanned = %d, hashed = %d, want 3 and 2", res.Scanned, res.Hashed)
	}
	if got := e.stored(t); !equal(got, []string{"a.txt", "sub", "sub/b.txt"}) {
		t.Fatalf("stored = %v", got)
	}
	if e.events.Count("scan.completed") != 1 {
		t.Fatalf("scan.completed events = %d, want 1", e.events.Count("scan.completed"))
	}
}

func TestScan_ModifyDetectedByMtime(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	e.overwrite(t, "a.txt", "alpha changed")
	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"modify:a.txt"}) {
		t.Fatalf("changes = %v, want one modify", got)
	}
	if res.Hashed != 1 {
		t.Fatalf("hashed = %d, want 1", res.Hashed)
	}
}

func TestScan_ReusesHashWhenUnchanged(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write("b.txt", "beta")
	e.scan(t, scan.Options{})

	e.hasher.calls = 0
	res := e.scan(t, scan.Options{})

	if len(res.Changes) != 0 {
		t.Fatalf("changes = %v, want none", summary(res))
	}
	if e.hasher.calls != 0 || res.Hashed != 0 {
		t.Fatalf("hasher calls = %d, hashed = %d, want 0", e.hasher.calls, res.Hashed)
	}
}

func TestScan_Delete(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write("b.txt", "beta")
	e.scan(t, scan.Options{})

	e.remove(t, "a.txt")
	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"delete:a.txt"}) {
		t.Fatalf("changes = %v, want one delete", got)
	}
	if got := e.stored(t); !equal(got, []string{"b.txt"}) {
		t.Fatalf("stored = %v, want only b.txt", got)
	}
}

func TestScan_RenameSameHash(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	if err := e.fs.Rename(root+"/a.txt", root+"/b.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"rename:b.txt<-a.txt"}) {
		t.Fatalf("changes = %v, want one rename", got)
	}
	// The hash cache is keyed by path, so the file at its new path is read
	// again; the identical hash is what makes the pair a rename.
	if res.Hashed != 1 {
		t.Fatalf("hashed = %d, want 1", res.Hashed)
	}
	if got := e.stored(t); !equal(got, []string{"b.txt"}) {
		t.Fatalf("stored = %v, want only b.txt", got)
	}
}

func TestScan_EditorAtomicSaveIsModify(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	// Write a temporary file next to the target, then rename over it.
	e.write("a.txt.tmp", "alpha rewritten")
	if err := e.fs.Rename(root+"/a.txt.tmp", root+"/a.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"modify:a.txt"}) {
		t.Fatalf("changes = %v, want one modify", got)
	}
}

func TestScan_HiddenSkippedByDefault(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write(".hidden", "secret")
	e.write(".cache/x.txt", "cached")

	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"create:a.txt"}) {
		t.Fatalf("changes = %v, want only a.txt", got)
	}
	if res.Ignored != 2 {
		t.Fatalf("ignored = %d, want 2", res.Ignored)
	}
}

func TestScan_HiddenIncludedWithPolicy(t *testing.T) {
	e := newEnv(t, func(f *folder.Folder) { f.IncludeHidden = true })
	e.write("a.txt", "alpha")
	e.write(".hidden", "secret")

	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"create:.hidden", "create:a.txt"}) {
		t.Fatalf("changes = %v", got)
	}
}

func TestScan_SymlinkSkip(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.fs.AddSymlink(root+"/link", "a.txt")

	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"create:a.txt"}) {
		t.Fatalf("changes = %v, want only a.txt", got)
	}
}

func TestScan_SymlinkStore(t *testing.T) {
	e := newEnv(t, func(f *folder.Folder) { f.Symlinks = folder.SymlinkStore })
	e.fs.AddSymlink(root+"/link", "a.txt")

	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"create:link"}) {
		t.Fatalf("changes = %v, want one create", got)
	}
	m, err := e.store.LoadManifest(context.Background(), e.folder.ID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	entry := m.Entries["link"]
	if entry.Kind != manifest.KindSymlink || entry.LinkTarget != "a.txt" {
		t.Fatalf("stored entry = %+v, want symlink to a.txt", entry)
	}
}

func TestScan_EmptyDirKeptOrDropped(t *testing.T) {
	t.Run("kept", func(t *testing.T) {
		e := newEnv(t)
		e.fs.AddDir(root+"/empty", 0o755)

		res := e.scan(t, scan.Options{})

		if got := summary(res); !equal(got, []string{"create:empty"}) {
			t.Fatalf("changes = %v, want the directory", got)
		}
	})

	t.Run("dropped", func(t *testing.T) {
		e := newEnv(t, func(f *folder.Folder) { f.KeepEmptyDirs = false })
		e.fs.AddDir(root+"/empty", 0o755)
		e.write("sub/a.txt", "alpha")

		res := e.scan(t, scan.Options{})

		if got := summary(res); !equal(got, []string{"create:sub/a.txt"}) {
			t.Fatalf("changes = %v, want only the file", got)
		}
	})
}

func TestScan_IgnoreFileAndConfiguredPatterns(t *testing.T) {
	e := newEnv(t, func(f *folder.Folder) { f.IgnorePatterns = []string{"*.tmp", "build/"} })
	e.write("a.txt", "alpha")
	e.write("junk.tmp", "junk")
	e.write("build/out.bin", "binary")
	e.write("notes.log", "log")
	e.write("keep.tmp", "kept")
	e.fs.AddFile(root+"/"+scan.IgnoreFile, []byte("# ignore file\n*.log\n!keep.tmp\n"), 0o644)

	res := e.scan(t, scan.Options{})

	want := []string{"create:a.txt", "create:keep.tmp"}
	if got := summary(res); !equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

func TestScan_SyncthisDirAlwaysIgnored(t *testing.T) {
	e := newEnv(t, func(f *folder.Folder) {
		f.IncludeHidden = true
		f.IgnorePatterns = []string{"!.syncthis/"}
	})
	e.write("a.txt", "alpha")
	e.write(".syncthis/tmp/part.bin", "partial")

	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"create:a.txt"}) {
		t.Fatalf("changes = %v, want only a.txt", got)
	}
}

func TestScan_DryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")

	res := e.scan(t, scan.Options{DryRun: true})

	if got := summary(res); !equal(got, []string{"create:a.txt"}) {
		t.Fatalf("changes = %v, want one create", got)
	}
	if !res.DryRun {
		t.Fatal("result should be marked as a dry run")
	}
	if got := e.stored(t); len(got) != 0 {
		t.Fatalf("stored = %v, want nothing written", got)
	}
	f, err := e.store.GetFolder(context.Background(), e.folder.ID)
	if err != nil {
		t.Fatalf("get folder: %v", err)
	}
	if !f.LastScanAt.IsZero() {
		t.Fatalf("last scan = %v, want unset after a dry run", f.LastScanAt)
	}
}

func TestScan_PartialSubtreeOnlyTouchesPrefix(t *testing.T) {
	e := newEnv(t)
	e.write("keep/a.txt", "alpha")
	e.write("work/b.txt", "beta")
	e.scan(t, scan.Options{})

	e.remove(t, "keep/a.txt")
	e.write("work/c.txt", "gamma")
	res := e.scan(t, scan.Options{Paths: []string{"work"}})

	if got := summary(res); !equal(got, []string{"create:work/c.txt"}) {
		t.Fatalf("changes = %v, want only the new file under work", got)
	}
	if got := e.stored(t); !equal(got, []string{"keep", "keep/a.txt", "work", "work/b.txt", "work/c.txt"}) {
		t.Fatalf("stored = %v, want keep/a.txt untouched", got)
	}

	// A subtree that disappeared deletes everything stored beneath it.
	e.remove(t, "keep")
	res = e.scan(t, scan.Options{Paths: []string{"keep"}})
	if got := summary(res); !equal(got, []string{"delete:keep", "delete:keep/a.txt"}) {
		t.Fatalf("changes = %v, want the subtree deleted", got)
	}
}

func TestScan_UnicodeNames(t *testing.T) {
	e := newEnv(t)
	e.write("résumé.txt", "cv")
	e.write("日本/ファイル.txt", "file")

	res := e.scan(t, scan.Options{})

	want := []string{"create:résumé.txt", "create:日本", "create:日本/ファイル.txt"}
	sort.Strings(want)
	if got := summary(res); !equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

func TestScan_CancelMidwayLeavesDBConsistent(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		e.write(name, strings.Repeat(name, 4))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.hasher.before = func(n int) {
		if n == 2 {
			cancel()
		}
	}

	if _, err := e.scanner.Scan(ctx, e.folder, scan.Options{}); err == nil {
		t.Fatal("scan should fail when the context is cancelled")
	}
	if got := e.stored(t); len(got) != 0 {
		t.Fatalf("stored = %v, want nothing: persistence happens after the walk", got)
	}

	e.hasher.before = nil
	res := e.scan(t, scan.Options{})
	if got := e.stored(t); !equal(got, []string{"a.txt", "b.txt", "c.txt", "d.txt"}) {
		t.Fatalf("stored after the second scan = %v (changes %v)", got, summary(res))
	}
}

// compile-time check that the counting hasher still satisfies the port.
var _ ports.Hasher = (*countingHasher)(nil)
