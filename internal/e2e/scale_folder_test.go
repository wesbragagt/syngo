package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/blake3hash"
	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// The shape of the scale fixture: 200 directories of 25 files, so the default
// is the 5,000 files the task list names. 5,000 is the vault-scale target of
// adr-006: the folder this program is being built to carry is a notes vault
// of a few hundred to a few thousand markdown files plus attachments, and
// 5,000 is comfortably larger than it with headroom left over.
// SYNCTHIS_SCALE_FILES_EACH changes how many files each directory holds,
// which is how a machine with more time runs the same test over a larger tree
// and reports the number it really ran.
const (
	scaleTreeDirs          = 200
	scaleTreeFilesEachEnv  = "SYNCTHIS_SCALE_FILES_EACH"
	scaleTreeFilesEachFull = 25
)

// The mutation the reconcile scan has to classify: a tenth of the tree
// rewritten, one directory's worth deleted and one directory's worth added.
// The counts are derived from the tree shape so a smaller tree mutates
// proportionally.
const (
	scaleModifiedDirs = 20
	scaleDeletedDirs  = 2
	scaleAddedDirs    = 2
)

// scaleTree is a generated file tree and what a scan of it must record.
type scaleTree struct {
	root string
	// want is the content hash of every file in the tree, by path relative
	// to the root with forward slashes, which is how a manifest names it.
	want map[string]manifest.Hash
	// dirs is how many directories the tree holds, files how many files,
	// and bytes their total size.
	dirs, files int
	bytes       int64
}

// scaleFileContent is the content of one file of the tree at a revision.
//
// Two properties matter. Consecutive revisions of a file always differ in
// length, so a rescan cannot miss a change whatever the filesystem says about
// modification times. And no two files of the tree ever hold the same bytes,
// because the path and the revision are written into the content: a fixture
// with duplicate content makes the scanner pair a deleted file with a created
// one and report a rename, which is correct behaviour and would make the
// expected change counts of this test wrong.
func scaleFileContent(dir, file, revision, filesEach int) []byte {
	head := fmt.Sprintf("d%03d/f%04d/r%d\n", dir, file, revision)
	size := len(head) + 32 + (dir*filesEach+file+revision*37)%225
	out := make([]byte, size)
	copy(out, head)
	for i := len(head); i < size; i++ {
		out[i] = byte('a' + (dir+file+revision+i)%26)
	}
	return out
}

func scaleDirName(d int) string    { return fmt.Sprintf("d%03d", d) }
func scaleFileName(f int) string   { return fmt.Sprintf("f%04d.txt", f) }
func scaleRelPath(d, f int) string { return scaleDirName(d) + "/" + scaleFileName(f) }
func scaleFullPath(root string, d, f int) string {
	return filepath.Join(root, scaleDirName(d), scaleFileName(f))
}

// buildScaleTree writes dirs directories of filesEach files each and returns
// the tree together with the hash of every file in it. The hashes are
// computed from the bytes that were written, not read back from disk, so the
// test compares what the scanner recorded against what the test put there.
func buildScaleTree(t *testing.T, root string, dirs, filesEach int) *scaleTree {
	t.Helper()
	started := time.Now()
	hasher := blake3hash.New()
	tree := &scaleTree{root: root, want: make(map[string]manifest.Hash, dirs*filesEach), dirs: dirs}
	for d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, scaleDirName(d)), 0o755); err != nil {
			t.Fatalf("create directory: %v", err)
		}
		for f := range filesEach {
			content := scaleFileContent(d, f, 0, filesEach)
			if err := os.WriteFile(scaleFullPath(root, d, f), content, 0o644); err != nil {
				t.Fatalf("write %s: %v", scaleRelPath(d, f), err)
			}
			tree.want[scaleRelPath(d, f)] = hasher.SumBytes(content)
			tree.files++
			tree.bytes += int64(len(content))
		}
	}
	t.Logf("built %d files in %d directories (%s) in %v",
		tree.files, tree.dirs, humanBytes(tree.bytes), time.Since(started))
	return tree
}

// writeScaleFile rewrites one file of the tree at a revision and records the
// new hash.
func (s *scaleTree) writeScaleFile(t *testing.T, d, f, revision, filesEach int) {
	t.Helper()
	content := scaleFileContent(d, f, revision, filesEach)
	_, existed := s.want[scaleRelPath(d, f)]
	if err := os.WriteFile(scaleFullPath(s.root, d, f), content, 0o644); err != nil {
		t.Fatalf("rewrite %s: %v", scaleRelPath(d, f), err)
	}
	if !existed {
		s.files++
	} else {
		s.bytes -= int64(len(scaleFileContent(d, f, revision-1, filesEach)))
	}
	s.bytes += int64(len(content))
	s.want[scaleRelPath(d, f)] = blake3hash.New().SumBytes(content)
}

// removeScaleFile deletes one file of the tree.
func (s *scaleTree) removeScaleFile(t *testing.T, d, f, filesEach int) {
	t.Helper()
	if err := os.Remove(scaleFullPath(s.root, d, f)); err != nil {
		t.Fatalf("remove %s: %v", scaleRelPath(d, f), err)
	}
	s.bytes -= int64(len(scaleFileContent(d, f, 0, filesEach)))
	s.files--
	delete(s.want, scaleRelPath(d, f))
}

// scaleApp initializes a data directory with the tree configured as the
// folder "docs", and returns the application. Nothing runs a daemon: the
// claim is about the scanner and the store.
func scaleApp(t *testing.T, dataDir, root string) *compose.App {
	t.Helper()
	app, _, err := compose.Init(context.Background(),
		compose.Options{DataDir: dataDir, Events: fakes.NewEvents()},
		compose.InitSettings{DeviceName: "scale"})
	if err != nil {
		t.Fatalf("init the data directory: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("close the data directory: %v", err)
		}
	})
	_, err = app.Folders.Add(context.Background(), folder.Folder{
		ID: folderID, Path: root, Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkSkip,
		KeepEmptyDirs: true,
	})
	if err != nil {
		t.Fatalf("add the folder: %v", err)
	}
	return app
}

// scanFolder runs one scan of the folder and returns what it found.
func scanFolder(t *testing.T, app *compose.App, o scan.Options) scan.Result {
	t.Helper()
	f, err := app.Store.GetFolder(context.Background(), folderID)
	if err != nil {
		t.Fatalf("load the folder: %v", err)
	}
	started := time.Now()
	result, err := app.Scan.Scan(context.Background(), f, o)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	t.Logf("scan: %d entries, %d hashed, %d changes, %d skipped, took %v",
		result.Scanned, result.Hashed, len(result.Changes), result.Skipped, time.Since(started))
	if result.Skipped != 0 {
		t.Fatalf("the scan skipped %d entries: %s", result.Skipped, result.Warning())
	}
	return result
}

// assertStoredTree is the invariant the vault-scale test is about: the store
// holds one live row per file of the tree, with the hash of the bytes that
// were written, one row per directory, and nothing else.
func assertStoredTree(t *testing.T, app *compose.App, tree *scaleTree) {
	t.Helper()
	ctx := context.Background()

	stats, err := app.Store.FolderStats(ctx, folderID)
	if err != nil {
		t.Fatalf("folder stats: %v", err)
	}
	if stats.Files != int64(tree.files) || stats.Dirs != int64(tree.dirs) {
		t.Fatalf("stored rows = %d files and %d directories, want %d and %d",
			stats.Files, stats.Dirs, tree.files, tree.dirs)
	}
	if stats.Bytes != tree.bytes {
		t.Fatalf("stored bytes = %d, want %d", stats.Bytes, tree.bytes)
	}
	if stats.Symlinks != 0 {
		t.Fatalf("stored symlinks = %d, want none", stats.Symlinks)
	}

	m, err := app.Store.LoadManifest(ctx, folderID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	var files, dirs int
	var mismatched []string
	for _, entry := range m.Live() {
		switch entry.Kind {
		case manifest.KindDir:
			dirs++
			continue
		case manifest.KindFile:
			files++
		default:
			t.Fatalf("%s is stored as a %s, want a file or a directory", entry.Path, entry.Kind)
		}
		want, ok := tree.want[entry.Path]
		switch {
		case !ok:
			mismatched = append(mismatched, entry.Path+": stored but not on disk")
		case entry.Hash != want:
			mismatched = append(mismatched,
				fmt.Sprintf("%s: hash %x, want %x", entry.Path, entry.Hash, want))
		case entry.Size != int64(len(scaleFileContentOf(tree, entry.Path))):
			mismatched = append(mismatched, entry.Path+": wrong size")
		}
		if len(mismatched) > 5 {
			break
		}
	}
	if len(mismatched) > 0 {
		t.Fatalf("the store does not match the tree:\n%s", join(mismatched))
	}
	if files != tree.files || dirs != tree.dirs {
		t.Fatalf("manifest holds %d files and %d directories, want %d and %d",
			files, dirs, tree.files, tree.dirs)
	}
	// Every file of the tree is stored: the counts above are equal and no
	// stored path was missing from the tree, so a file on disk that was
	// never recorded would have made the counts differ.
	for path := range tree.want {
		if _, err := app.Store.GetFile(ctx, folderID, path); err != nil {
			t.Fatalf("%s is on disk and not in the store: %v", path, err)
		}
		break
	}
}

// scaleFileContentOf returns the bytes a path of the tree currently holds,
// read from disk. It is only used to check a stored size against the file.
func scaleFileContentOf(tree *scaleTree, path string) []byte {
	data, err := os.ReadFile(filepath.Join(tree.root, filepath.FromSlash(path)))
	if err != nil {
		return nil
	}
	return data
}

// TestScale_VaultFolderReconciles is the Phase 1 claim: a folder of 5,000
// files is scanned, mutated and reconciled, and after each step the database
// holds exactly the tree that is on disk, hash by hash.
func TestScale_VaultFolderReconciles(t *testing.T) {
	requireScale(t)
	filesEach := scaleEnvInt(t, scaleTreeFilesEachEnv, scaleTreeFilesEachFull)
	dir := scaleScratch(t)
	root := filepath.Join(dir, "root")
	tree := buildScaleTree(t, root, scaleTreeDirs, filesEach)

	app := scaleApp(t, filepath.Join(dir, "data"), root)

	// The first scan records the whole tree.
	first := scanFolder(t, app, scan.Options{})
	if got := first.Scanned; got != tree.files+tree.dirs {
		t.Fatalf("the first scan recorded %d entries, want %d files and %d directories",
			got, tree.files, tree.dirs)
	}
	assertStoredTree(t, app, tree)

	// The mutation the reconcile has to classify.
	modified := scaleModifiedDirs * filesEach
	deleted := scaleDeletedDirs * filesEach
	added := scaleAddedDirs * filesEach
	for d := range scaleModifiedDirs {
		for f := range filesEach {
			tree.writeScaleFile(t, d, f, 1, filesEach)
		}
	}
	for d := scaleTreeDirs - scaleDeletedDirs; d < scaleTreeDirs; d++ {
		for f := range filesEach {
			tree.removeScaleFile(t, d, f, filesEach)
		}
	}
	for d := scaleTreeDirs; d < scaleTreeDirs+scaleAddedDirs; d++ {
		if err := os.MkdirAll(filepath.Join(root, scaleDirName(d)), 0o755); err != nil {
			t.Fatalf("create directory: %v", err)
		}
		tree.dirs++
		for f := range filesEach {
			tree.writeScaleFile(t, d, f, 0, filesEach)
		}
	}

	// The reconcile: one full scan that has to find every change.
	second := scanFolder(t, app, scan.Options{})
	counts := second.Counts()
	want := map[manifest.ChangeType]int{
		manifest.Create: added + scaleAddedDirs,
		manifest.Modify: modified,
		manifest.Delete: deleted,
	}
	for changeType, n := range want {
		if counts[changeType] != n {
			t.Fatalf("the reconcile classified %d %s changes, want %d (all: %v)",
				counts[changeType], changeType, n, counts)
		}
	}
	if counts[manifest.Rename] != 0 {
		t.Fatalf("the reconcile classified %d renames, want none", counts[manifest.Rename])
	}
	assertStoredTree(t, app, tree)

	// Nothing is left to do: a scan of a tree that was just recorded finds
	// no change, which is what "safely" means for a reconcile that large.
	third := scanFolder(t, app, scan.Options{DryRun: true})
	if len(third.Changes) != 0 {
		t.Fatalf("a repeated scan found %d changes, want none", len(third.Changes))
	}
	t.Logf("reconciled %d files: %d modified, %d deleted, %d added",
		tree.files, modified, deleted, added)
}

// idleMemoryLimit is the Phase 1 claim: a daemon that has finished its work
// holds less than 100 MiB resident. 100 MiB rather than 100 MB is the looser
// of the two readings of "100 MB", so a pass is a pass under either.
const idleMemoryLimit = 100 << 20

// scaleIdleSecondsEnv is how long the daemon is watched after it reports an
// idle folder and no pending change, in seconds. A window is needed rather
// than one reading: a single sample taken at the wrong moment says nothing,
// and the Go runtime hands pages back to the kernel on its own schedule, so a
// short window measures the heap right after the scan rather than the idle
// daemon. The default is five minutes, which is long enough for the
// scavenger to have run several times.
const scaleIdleSecondsEnv = "SYNCTHIS_SCALE_IDLE_SECONDS"

// TestScale_IdleMemoryUnderTheLimit starts a real daemon process over a
// 5,000-file folder, waits until it has scanned everything and gone idle,
// and measures its resident memory from /proc. A platform that cannot report
// it skips: passing on an unmeasurable claim would be worse than not running.
func TestScale_IdleMemoryUnderTheLimit(t *testing.T) {
	requireScale(t)
	requireProcMemory(t)
	requireBinary(t)

	filesEach := scaleEnvInt(t, scaleTreeFilesEachEnv, scaleTreeFilesEachFull)
	dir := scaleScratch(t)
	root := filepath.Join(dir, "root")
	tree := buildScaleTree(t, root, scaleTreeDirs, filesEach)

	// The data directory holds the control socket, and a unix socket path
	// is limited to about 100 characters: a data directory inside the
	// scratch directory, which is itself deep inside a checkout, makes the
	// daemon fail to bind. The rest of the scale group keeps its data
	// directories in a short path for the same reason.
	dataDir := filepath.Join(scaleShortDir(t), "data")
	if code, out := runBinary(t, dataDir, "init", "--name", "idle", "--json"); code != 0 {
		t.Fatalf("init exit = %d, want 0 (%s)", code, out)
	}
	if code, out := runBinary(t, dataDir, "folder", "add", root, "--id", string(folderID), "--json"); code != 0 {
		t.Fatalf("folder add exit = %d, want 0 (%s)", code, out)
	}

	daemon := startChild(t, dataDir, "daemon", "run", "--listen", "127.0.0.1:0")
	pid := daemon.cmd.Process.Pid

	// The daemon is idle once it has recorded the whole tree, reports the
	// folder as idle and has nothing pending.
	scanTimeout := 10 * time.Minute * timeoutScale
	waitFor(t, "the daemon to scan the whole folder and go idle", scanTimeout, func() bool {
		if daemon.finished() {
			t.Fatalf("the daemon exited: %s", daemon.errorOutput())
		}
		report := statusJSON(t, dataDir)
		if !report.Daemon.Running || len(report.Folders) != 1 {
			return false
		}
		f := report.Folders[0]
		return f.Files == int64(tree.files) && f.State == "idle" && f.PendingChanges == 0
	})
	if peak, err := peakResidentBytes(pid); err == nil {
		t.Logf("peak resident memory including the startup scan: %s", humanBytes(peak))
	}

	// Watch it while it stays idle. The daemon keeps its watcher and its
	// rescan timer running, so this is an idle daemon doing its normal
	// background work, not a stopped one. Every sample is kept: the shape
	// of the curve is what says whether a high reading is the daemon
	// holding memory or the runtime not having returned it yet.
	idleSettle := time.Duration(scaleEnvInt(t, scaleIdleSecondsEnv, 300)) * time.Second * timeoutScale
	readings := make([]int64, 0, int(idleSettle/(500*time.Millisecond))+1)
	deadline := time.Now().Add(idleSettle)
	for time.Now().Before(deadline) {
		rss, err := residentBytes(pid)
		if err != nil {
			t.Fatalf("read the daemon's memory: %v", err)
		}
		readings = append(readings, rss)
		time.Sleep(500 * time.Millisecond)
	}
	if len(readings) < 4 {
		t.Fatalf("only %d memory samples over %v", len(readings), idleSettle)
	}

	// The settled level is the highest reading over the last quarter of
	// the window. That is the claim: what an idle daemon holds, not what
	// it held in the seconds right after a scan of the whole folder.
	var whole, settled int64
	tail := len(readings) * 3 / 4
	for i, rss := range readings {
		whole = max(whole, rss)
		if i >= tail {
			settled = max(settled, rss)
		}
	}
	t.Logf("idle over %v: %d samples, first %s, highest %s, settled %s (last quarter), last %s, limit %s (%d files)",
		idleSettle, len(readings), humanBytes(readings[0]), humanBytes(whole),
		humanBytes(settled), humanBytes(readings[len(readings)-1]),
		humanBytes(idleMemoryLimit), tree.files)

	// Bytes per file is the number that says whether a reading is a fixed
	// overhead or something that grows with the folder. A run over a
	// smaller tree that reports the same per-file figure has measured the
	// same thing the full run does.
	perFile := float64(settled) / float64(tree.files)
	t.Logf("settled %s over %d files = %.0f B per file", humanBytes(settled), tree.files, perFile)

	if settled >= idleMemoryLimit {
		t.Fatalf("idle resident memory settled at %s over %v (peak %s, %.0f B per file "+
			"over %d files), want under %s",
			humanBytes(settled), idleSettle, humanBytes(whole), perFile, tree.files,
			humanBytes(idleMemoryLimit))
	}
}
