package scan_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// root is the folder path every test scans.
const root = "/docs"

// testChunkSize is the average chunk size these scans cut with. It is small so
// a test fixture of a few kilobytes is still cut into several chunks.
const testChunkSize = 4 << 10

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

// Stream returns an incremental digest, counting the call. The scan reads a
// file exactly once, cutting and hashing it as it streams past, so one stream
// is one file read: the same event Sum used to count.
func (h *countingHasher) Stream() ports.Stream {
	h.calls++
	if h.before != nil {
		h.before(h.calls)
	}
	return h.inner.Stream()
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
	e.scanner = scan.New(e.fs, e.hasher, e.store, e.clock, fakes.NewIdentity("DEV1", "test"), e.events, testChunkSize, nil)
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

// unreadableFS is a filesystem where one path cannot be opened, like a file
// whose permissions deny the user running the scan.
type unreadableFS struct {
	*fakes.FileSystem
	path string
}

// Open fails with a permission error for the unreadable path.
func (f unreadableFS) Open(p string) (io.ReadCloser, error) {
	if p == f.path {
		return nil, fmt.Errorf("open %s: %w", p, fs.ErrPermission)
	}
	return f.FileSystem.Open(p)
}

// scannerOver returns a scanner using fsys instead of the environment's own
// filesystem, with everything else shared.
func (e *env) scannerOver(fsys ports.FileSystem) *scan.Scanner {
	return scan.New(fsys, e.hasher, e.store, e.clock, fakes.NewIdentity("DEV1", "test"), e.events, testChunkSize, nil)
}

// folderState returns the stored scan state of the environment's folder.
func (e *env) folderState(t *testing.T) (time.Time, string) {
	t.Helper()
	f, err := e.store.GetFolder(context.Background(), e.folder.ID)
	if err != nil {
		t.Fatalf("get folder: %v", err)
	}
	return f.LastScanAt, f.LastError
}

func TestScan_UnreadableFileIsSkippedAndRecorded(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write("locked", "secret")
	scanner := e.scannerOver(unreadableFS{e.fs, root + "/locked"})

	res, err := scanner.Scan(context.Background(), e.folder, scan.Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if res.Skipped != 1 || len(res.Warnings) != 1 {
		t.Fatalf("skipped = %d, warnings = %v, want 1 and one message", res.Skipped, res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "locked") || !strings.Contains(res.Warnings[0], "permission denied") {
		t.Fatalf("warning = %q", res.Warnings[0])
	}
	if got := summary(res); !equal(got, []string{"create:a.txt"}) {
		t.Fatalf("changes = %v, want only the readable file", got)
	}
	if got := e.stored(t); !equal(got, []string{"a.txt"}) {
		t.Fatalf("stored = %v, want only the readable file", got)
	}

	at, lastErr := e.folderState(t)
	if lastErr != res.Warning() || lastErr == "" {
		t.Fatalf("folder last error = %q, want the scan warning %q", lastErr, res.Warning())
	}
	if at.IsZero() {
		t.Fatal("a scan that finished must record when it ran")
	}
}

func TestScan_UnreadableFileKeepsItsStoredVersion(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write("locked", "secret")
	e.scan(t, scan.Options{})

	scanner := e.scannerOver(unreadableFS{e.fs, root + "/locked"})
	e.overwrite(t, "locked", "changed")

	res, err := scanner.Scan(context.Background(), e.folder, scan.Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("changes = %v, want none: an unreadable file is not a deletion", summary(res))
	}
	if got := e.stored(t); !equal(got, []string{"a.txt", "locked"}) {
		t.Fatalf("stored = %v, want both files", got)
	}
}

func TestScan_WalkFailureIsRecordedOnTheFolder(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})
	firstScanAt, _ := e.folderState(t)

	scanner := e.scannerOver(brokenFS{e.fs})
	if _, err := scanner.Scan(context.Background(), e.folder, scan.Options{}); err == nil {
		t.Fatal("scan should fail when the folder cannot be walked")
	}

	at, lastErr := e.folderState(t)
	if lastErr == "" {
		t.Fatal("a failed scan must record its error on the folder")
	}
	if !at.Equal(firstScanAt) {
		t.Fatalf("last scan time = %v, want the time of the last scan that finished (%v)", at, firstScanAt)
	}
}

// brokenFS is a filesystem whose walk always fails.
type brokenFS struct {
	*fakes.FileSystem
}

// Walk reports an unreadable directory.
func (f brokenFS) Walk(_ context.Context, root string, _ ports.WalkFunc) error {
	return fmt.Errorf("walk %s: %w", root, fs.ErrPermission)
}

// hookedStore runs a function the first time a manifest is loaded, so a test
// can let another scan write in between a scan's load and its persist.
type hookedStore struct {
	*fakes.Store
	once  sync.Once
	after func()
}

// LoadManifest loads the manifest and then runs the hook once.
func (s *hookedStore) LoadManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error) {
	m, err := s.Store.LoadManifest(ctx, id)
	s.once.Do(func() {
		if s.after != nil {
			s.after()
		}
	})
	return m, err
}

func TestScan_ConcurrentScansLogEachChangeOnce(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write("sub/b.txt", "beta")

	hooked := &hookedStore{Store: e.store}
	slow := scan.New(e.fs, e.hasher, hooked, e.clock, fakes.NewIdentity("DEV1", "test"), e.events, testChunkSize, nil)
	hooked.after = func() {
		// A second scanner finishes completely while the first one is still
		// walking the folder.
		if _, err := e.scanner.Scan(context.Background(), e.folder, scan.Options{}); err != nil {
			t.Errorf("concurrent scan: %v", err)
		}
	}

	res, err := slow.Scan(context.Background(), e.folder, scan.Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("changes = %v, want none: the other scan logged them already", summary(res))
	}

	recs, err := e.store.RecentChanges(context.Background(), e.folder.ID, 0)
	if err != nil {
		t.Fatalf("recent changes: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("change rows = %d, want 3 (one per created entry)", len(recs))
	}
	if got := e.stored(t); !equal(got, []string{"a.txt", "sub", "sub/b.txt"}) {
		t.Fatalf("stored = %v", got)
	}
}

func TestScan_MissingRootFailsWithoutDeletingAnything(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.write("sub/b.txt", "beta")
	e.scan(t, scan.Options{})
	firstScanAt, _ := e.folderState(t)

	e.remove(t, "sub/b.txt")
	e.remove(t, "a.txt")
	e.remove(t, "sub")
	if err := e.fs.Remove(root); err != nil {
		t.Fatalf("remove root: %v", err)
	}

	_, err := e.scanner.Scan(context.Background(), e.folder, scan.Options{})
	if !errors.Is(err, scan.ErrRootMissing) {
		t.Fatalf("scan error = %v, want ErrRootMissing", err)
	}
	if got := e.stored(t); !equal(got, []string{"a.txt", "sub", "sub/b.txt"}) {
		t.Fatalf("stored = %v, want the entries untouched", got)
	}

	at, lastErr := e.folderState(t)
	if !strings.Contains(lastErr, "root") {
		t.Fatalf("last error = %q, want it to name the missing root", lastErr)
	}
	if !at.Equal(firstScanAt) {
		t.Fatalf("last scan time = %v, want the time of the last scan that finished (%v)", at, firstScanAt)
	}
}

func TestScan_RootThatIsNotADirectoryFails(t *testing.T) {
	e := newEnv(t)
	if err := e.fs.Remove(root); err != nil {
		t.Fatalf("remove root: %v", err)
	}
	e.fs.AddFile(root, []byte("not a directory"), 0o644)

	if _, err := e.scanner.Scan(context.Background(), e.folder, scan.Options{}); !errors.Is(err, scan.ErrRootMissing) {
		t.Fatalf("scan error = %v, want ErrRootMissing", err)
	}
}

// entry returns one stored entry, tombstones included.
func (e *env) entry(t *testing.T, p string) manifest.Entry {
	t.Helper()
	stored, err := e.store.GetFile(context.Background(), e.folder.ID, p)
	if err != nil {
		t.Fatalf("get %s: %v", p, err)
	}
	return stored
}

// wantCounter fails the test unless the stored entry's vector holds exactly
// the given counter for the scanning device.
func (e *env) wantCounter(t *testing.T, p string, want uint64) {
	t.Helper()
	got := e.entry(t, p).Version
	if got.Counter("DEV1") != want {
		t.Fatalf("version of %s = %q, want the counter of DEV1 at %d", p, got, want)
	}
}

func TestScan_CreateStartsTheVersionAtOne(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	e.wantCounter(t, "a.txt", 1)
	if by := e.entry(t, "a.txt").ModifiedBy; by != "DEV1" {
		t.Fatalf("author = %q, want the scanning device", by)
	}
}

func TestScan_ModifyBumpsTheVersion(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	e.overwrite(t, "a.txt", "alpha two")
	e.scan(t, scan.Options{})

	e.wantCounter(t, "a.txt", 2)
}

func TestScan_ModeOnlyChangeDoesNotBumpTheVersion(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})
	before := e.entry(t, "a.txt").Version

	if err := e.fs.Chmod(root+"/a.txt", 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	res := e.scan(t, scan.Options{})

	if got := summary(res); !equal(got, []string{"modify:a.txt"}) {
		t.Fatalf("changes = %v, want the chmod classified as a modify", got)
	}
	got := e.entry(t, "a.txt")
	if got.Mode != 0o600 {
		t.Fatalf("mode = %o, want 600", got.Mode)
	}
	// Permission bits are not content, so they must not order two versions
	// against each other.
	if !got.Version.Equals(before) {
		t.Fatalf("version = %q, want it unchanged at %q", got.Version, before)
	}
}

func TestScan_DeleteAndRecreateKeepBumpingTheSameVector(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	e.remove(t, "a.txt")
	e.scan(t, scan.Options{})
	tombstone := e.entry(t, "a.txt")
	if !tombstone.Deleted {
		t.Fatalf("entry = %+v, want a tombstone", tombstone)
	}
	e.wantCounter(t, "a.txt", 2)

	// A recreated file dominates its own deletion, so a peer that still
	// holds the tombstone takes the new content.
	e.write("a.txt", "alpha again")
	e.scan(t, scan.Options{})
	e.wantCounter(t, "a.txt", 3)
	if e.entry(t, "a.txt").Deleted {
		t.Fatal("entry is still a tombstone after the file came back")
	}
}

func TestScan_RenameBumpsBothPaths(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	if err := e.fs.Rename(root+"/a.txt", root+"/b.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	res := e.scan(t, scan.Options{})
	if got := summary(res); !equal(got, []string{"rename:b.txt<-a.txt"}) {
		t.Fatalf("changes = %v, want a rename", got)
	}

	// Both counters are above the floor the first scan left, so a version
	// this device writes is never one a peer may already hold.
	e.wantCounter(t, "b.txt", 2)
	e.wantCounter(t, "a.txt", 2)
	if !e.entry(t, "a.txt").Deleted {
		t.Fatal("the old path is not a tombstone")
	}
}

func TestScan_PartialScanKeepsTheVersionsOfUntouchedSubtrees(t *testing.T) {
	e := newEnv(t)
	e.write("keep/a.txt", "alpha")
	e.write("sub/b.txt", "beta")
	e.scan(t, scan.Options{})
	before := e.entry(t, "keep/a.txt").Version

	e.overwrite(t, "sub/b.txt", "beta two")
	e.scan(t, scan.Options{Paths: []string{"sub"}})

	e.wantCounter(t, "sub/b.txt", 2)
	if got := e.entry(t, "keep/a.txt").Version; !got.Equals(before) {
		t.Fatalf("version outside the scanned subtree = %q, want %q", got, before)
	}
}

func TestScan_RetryAfterAConcurrentScanBumpsFromTheStoredVersion(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	// Both scanners see the same edit; the second one finishes first, so
	// the first has to re-diff against what that one wrote.
	e.overwrite(t, "a.txt", "alpha two")
	hooked := &hookedStore{Store: e.store}
	slow := scan.New(e.fs, e.hasher, hooked, e.clock, fakes.NewIdentity("DEV1", "test"), e.events, testChunkSize, nil)
	hooked.after = func() {
		if _, err := e.scanner.Scan(context.Background(), e.folder, scan.Options{}); err != nil {
			t.Errorf("concurrent scan: %v", err)
		}
	}
	if _, err := slow.Scan(context.Background(), e.folder, scan.Options{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// The retry raises the counter from the version the other scan wrote,
	// not from the stale one it loaded, so the file is at 2 and not at 3.
	e.wantCounter(t, "a.txt", 2)
}

// TestScan_HonoursAndRaisesTheCounterFloor is the hardening against a database
// restored from a backup: the counters the restored database holds are behind
// what the peers have, and the floor is what lifts the next version above
// them, so an edit made after the restore is never dominated by an older one.
func TestScan_HonoursAndRaisesTheCounterFloor(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	// What the first contact after a restore records: a peer holds version
	// 8 of this device's counter, eight versions the restored database
	// knows nothing about.
	if err := e.store.RaiseCounterFloor(ctx, "DEV1", 8); err != nil {
		t.Fatalf("raise floor: %v", err)
	}

	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})
	e.wantCounter(t, "a.txt", 9)

	floor, err := e.store.CounterFloor(ctx, "DEV1")
	if err != nil {
		t.Fatalf("counter floor: %v", err)
	}
	if floor != 9 {
		t.Fatalf("floor = %d, want the scan to have raised it to 9", floor)
	}

	// A deletion is raised over the floor as well, so the tombstone
	// dominates the version a peer still holds.
	e.remove(t, "a.txt")
	e.scan(t, scan.Options{})
	e.wantCounter(t, "a.txt", 10)
	if floor, _ = e.store.CounterFloor(ctx, "DEV1"); floor != 10 {
		t.Fatalf("floor = %d, want the deletion to have raised it to 10", floor)
	}
}

// TestScan_RetryKeepsWhatAnotherWriterInstalled is the case an eight-hour soak
// found. A scan reads the tree, and while it reads, the puller installs a file
// from a peer. The install moves the folder sequence on, so the scan's write is
// refused as stale and the scan re-diffs the tree it walked against the
// reloaded manifest. The tree it walked is older than the install and knows
// nothing of the new file, so a re-diff that trusts it turns the install into a
// deletion, which the device then hands to every peer: two files that were
// never deleted disappear from the other devices.
//
// A scan may only classify the paths it actually read against the state it
// actually diffed. What another writer changed under it is not its to
// classify.
func TestScan_RetryKeepsWhatAnotherWriterInstalled(t *testing.T) {
	e := newEnv(t)
	e.write("a/kept.bin", "from the peer, long ago")
	e.write("b/one.txt", "one")
	e.scan(t, scan.Options{})

	// The local change this scan is about, in the subtree it scans.
	e.write("b/two.txt", "two")

	// While the scan walks b/, the puller installs a file under a/, which
	// the walk neither sees nor is asked to look at.
	installed := manifest.Entry{
		Path: "a/installed.bin", Kind: manifest.KindFile, Size: 4, Mode: 0o644,
		Hash: e.hasher.SumBytes([]byte("peer")), ModTime: time.Unix(100, 0),
	}
	var once sync.Once
	e.hasher.before = func(int) {
		once.Do(func() {
			e.write("a/installed.bin", "peer")
			if err := e.store.UpsertEntries(context.Background(), e.folder.ID,
				[]manifest.Entry{installed}); err != nil {
				t.Errorf("install: %v", err)
			}
		})
	}

	res := e.scan(t, scan.Options{Paths: []string{"b"}})

	for _, c := range res.Changes {
		if c.Type == manifest.Delete || c.Type == manifest.Rename {
			t.Errorf("the scan classified %s:%s, a path another writer installed under it",
				c.Type, c.Path)
		}
	}
	if got := e.stored(t); !equal(got, []string{"a", "a/installed.bin", "a/kept.bin", "b", "b/one.txt", "b/two.txt"}) {
		t.Errorf("the store holds %v, want the installed file kept and the local change recorded", got)
	}
}

// TestScan_RetryLeavesTheChunkIndexOfWhatAnotherWriterWrote is the first of the
// two refinements the fix above needed. A scan that loses the write race defers
// the paths another writer rewrote, so the entry of such a path keeps the
// winner's version. The cut of that path the walk read describes the content
// that lost, so writing it would leave the chunk rows of one version under the
// entry of another: a peer asking for the file gets a cut whose hashes its
// bytes do not match, the pull fails and the path goes to repair.
func TestScan_RetryLeavesTheChunkIndexOfWhatAnotherWriterWrote(t *testing.T) {
	e := newEnv(t)
	e.write("note.txt", "the content the first scan recorded")
	e.scan(t, scan.Options{})

	// The walk reads this. It never reaches the store: the peer's version
	// below wins the race for the row.
	e.overwrite(t, "note.txt", "the content this walk reads, which loses")

	peer := []byte("the content the peer installed")
	winner := manifest.Entry{
		Path: "note.txt", Kind: manifest.KindFile, Size: int64(len(peer)), Mode: 0o644,
		Hash: e.hasher.SumBytes(peer), ModTime: time.Unix(500, 0),
	}
	winnerChunks := []transfer.Chunk{
		{Index: 0, Offset: 0, Length: len(peer), Hash: winner.Hash},
	}
	var once sync.Once
	e.hasher.before = func(int) {
		once.Do(func() {
			ctx := context.Background()
			if err := e.store.UpsertEntries(ctx, e.folder.ID, []manifest.Entry{winner}); err != nil {
				t.Errorf("install: %v", err)
			}
			ix := transfer.ChunkIndex{Hash: winner.Hash, Chunks: winnerChunks}
			if err := e.store.SetFileChunks(ctx, e.folder.ID, winner.Path, ix); err != nil {
				t.Errorf("index the install: %v", err)
			}
		})
	}

	e.scan(t, scan.Options{})

	stored, err := e.store.GetFile(context.Background(), e.folder.ID, "note.txt")
	if err != nil {
		t.Fatalf("get note.txt: %v", err)
	}
	if stored.Hash != winner.Hash {
		t.Fatalf("the entry of note.txt is %v, want the version the other writer installed", stored.Hash)
	}
	got, err := e.store.FileChunks(context.Background(), e.folder.ID, "note.txt")
	if err != nil {
		t.Fatalf("chunks of note.txt: %v", err)
	}
	if got.Hash != winner.Hash || len(got.Chunks) != 1 || got.Chunks[0].Hash != winner.Hash ||
		int64(got.Chunks[0].Length) != winner.Size {
		t.Errorf("the chunk index of note.txt is %v, want the cut of the entry the store holds (%v)",
			got, winnerChunks)
	}
}

// TestScan_RetryDefersARenameWhoseSourceAnotherWriterWrote is the second
// refinement. The walk sees a rename: the source gone, the target created with
// its bytes. Another writer rewrites the source row while the walk reads, so
// the source is not this scan's to classify and stays live in the tree, which
// leaves the create of the target with no delete to pair with. Recording that
// create alone makes the peers pull bytes they already hold and leaves a source
// row for a later scan to reap. The target waits instead, and the next scan,
// which sees both halves against the state that won, calls it one rename.
func TestScan_RetryDefersARenameWhoseSourceAnotherWriterWrote(t *testing.T) {
	e := newEnv(t)
	e.write("src.bin", "the bytes that move from one path to the other")
	e.scan(t, scan.Options{})

	// The rename on disk.
	e.remove(t, "src.bin")
	e.write("dst.bin", "the bytes that move from one path to the other")

	// While the walk reads the target, a concurrent scan rewrites the row
	// of the source: same content, a new sequence, so this scan's write is
	// refused and the source is one it may no longer classify.
	var once sync.Once
	e.hasher.before = func(int) {
		once.Do(func() {
			ctx := context.Background()
			src, err := e.store.GetFile(ctx, e.folder.ID, "src.bin")
			if err != nil {
				t.Errorf("get src.bin: %v", err)
				return
			}
			if err := e.store.UpsertEntries(ctx, e.folder.ID, []manifest.Entry{src}); err != nil {
				t.Errorf("rewrite src.bin: %v", err)
			}
		})
	}

	racing := e.scan(t, scan.Options{})
	for _, c := range racing.Changes {
		if c.Type == manifest.Create && c.Path == "dst.bin" {
			t.Errorf("the scan recorded %v, a copy of bytes the folder already holds at src.bin",
				summary(racing))
		}
	}

	e.hasher.before = nil
	next := e.scan(t, scan.Options{})
	if got := summary(next); !equal(got, []string{"rename:dst.bin<-src.bin"}) {
		t.Errorf("the next scan classified %v, want the move as one rename", got)
	}
	if got := e.stored(t); !equal(got, []string{"dst.bin"}) {
		t.Errorf("the store holds %v, want the source reaped by the rename", got)
	}
}
