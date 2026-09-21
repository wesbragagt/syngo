package repair_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/repair"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// root is the folder path every test repairs.
const root = "/docs"

// env is a repair service wired to in-memory fakes.
type env struct {
	fs      *fakes.FileSystem
	store   *fakes.Store
	hasher  fakes.Hasher
	clock   *fakes.Clock
	service *repair.Service
	folder  folder.Folder
}

// newEnv returns a repair service over an empty folder at /docs.
func newEnv(t *testing.T) *env {
	t.Helper()

	f := folder.Folder{
		ID: "docs", Path: root, Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkSkip, KeepEmptyDirs: true,
	}
	e := &env{
		fs:     fakes.NewFileSystem(),
		store:  fakes.NewStore(),
		hasher: fakes.NewHasher(),
		clock:  fakes.NewClock(time.Time{}),
		folder: f,
	}
	e.fs.AddDir(root, 0o755)
	if err := e.store.SaveFolder(context.Background(), f); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	e.service = repair.New(e.fs, e.hasher, e.store, e.clock, fakes.NewIdentity("DEV1", "test"))
	return e
}

// addFile writes a file and stores the matching manifest entry.
func (e *env) addFile(t *testing.T, rel, content string) manifest.Entry {
	t.Helper()
	full := root + "/" + rel
	e.fs.AddFile(full, []byte(content), 0o644)
	info, err := e.fs.Lstat(full)
	if err != nil {
		t.Fatalf("lstat %s: %v", full, err)
	}
	entry := manifest.Entry{
		Path: rel, Kind: manifest.KindFile, Size: info.Size, ModTime: info.ModTime,
		Mode: 0o644, Inode: info.Inode, Hash: e.hasher.SumBytes([]byte(content)),
	}
	if err := e.store.UpsertEntries(context.Background(), e.folder.ID, []manifest.Entry{entry}); err != nil {
		t.Fatalf("upsert %s: %v", rel, err)
	}
	return entry
}

// run repairs the folder and fails the test on error.
func (e *env) run(t *testing.T, dryRun bool) repair.Report {
	t.Helper()
	report, err := e.service.Run(context.Background(), e.folder.ID, dryRun)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	return report
}

// live returns the live entry paths currently stored.
func (e *env) live(t *testing.T) []string {
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

// entry returns one stored entry.
func (e *env) entry(t *testing.T, rel string) manifest.Entry {
	t.Helper()
	m, err := e.store.LoadManifest(context.Background(), e.folder.ID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m.Entries[rel]
}

// addTransfer stores an unfinished transfer for rel with the given temp path.
func (e *env) addTransfer(t *testing.T, id, rel, tempPath string) transfer.Transfer {
	t.Helper()
	entry := manifest.Entry{Path: rel, Kind: manifest.KindFile, Size: 8192, Hash: e.hasher.SumBytes([]byte(rel))}
	tr := transfer.NewInbound(transfer.ID(id), e.folder.ID, entry, "PEER1", transfer.MinChunkSize, tempPath, e.clock.Now())
	tr.State = transfer.StateActive
	if err := tr.MarkVerified(0, e.hasher.SumBytes([]byte("chunk")), e.clock.Now()); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	if err := e.store.SaveTransfer(context.Background(), tr); err != nil {
		t.Fatalf("save transfer: %v", err)
	}
	return tr
}

func TestRepair_MarksMissingFilesDeleted(t *testing.T) {
	e := newEnv(t)
	e.addFile(t, "a.txt", "alpha")
	e.addFile(t, "b.txt", "beta")
	if err := e.fs.Remove(root + "/a.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	report := e.run(t, false)

	if len(report.MissingDeleted) != 1 || report.MissingDeleted[0] != "a.txt" {
		t.Fatalf("missing = %v, want [a.txt]", report.MissingDeleted)
	}
	if got := e.live(t); len(got) != 1 || got[0] != "b.txt" {
		t.Fatalf("live entries = %v, want [b.txt]", got)
	}
	if !report.Changed() {
		t.Fatal("report should be marked as changed")
	}
}

func TestRepair_RehashesStale(t *testing.T) {
	e := newEnv(t)
	before := e.addFile(t, "a.txt", "alpha")

	// Replace the contents behind the store's back.
	e.fs.AddFile(root+"/a.txt", []byte("alpha rewritten"), 0o644)

	report := e.run(t, false)

	if len(report.Rehashed) != 1 || report.Rehashed[0] != "a.txt" {
		t.Fatalf("rehashed = %v, want [a.txt]", report.Rehashed)
	}
	got := e.entry(t, "a.txt")
	if got.Hash == before.Hash {
		t.Fatal("hash was not updated")
	}
	if want := e.hasher.SumBytes([]byte("alpha rewritten")); got.Hash != want {
		t.Fatalf("hash = %s, want %s", got.Hash, want)
	}
	if got.Size != int64(len("alpha rewritten")) {
		t.Fatalf("size = %d, want %d", got.Size, len("alpha rewritten"))
	}
}

func TestRepair_ResetsTransferWithoutTempFile(t *testing.T) {
	e := newEnv(t)
	tr := e.addTransfer(t, "t1", "big.iso", root+"/"+repair.TempDir+"/t1.part")

	report := e.run(t, false)

	if len(report.TransfersReset) != 1 || report.TransfersReset[0] != "t1" {
		t.Fatalf("reset = %v, want [t1]", report.TransfersReset)
	}
	stored, err := e.store.GetTransfer(context.Background(), tr.ID)
	if err != nil {
		t.Fatalf("get transfer: %v", err)
	}
	if stored.State != transfer.StatePending {
		t.Fatalf("state = %s, want pending", stored.State)
	}
	if len(stored.MissingChunks()) != len(stored.Chunks) {
		t.Fatalf("missing chunks = %d, want all %d", len(stored.MissingChunks()), len(stored.Chunks))
	}
}

func TestRepair_RemovesOrphanTemp(t *testing.T) {
	e := newEnv(t)
	tmp := root + "/" + repair.TempDir
	claimed := tmp + "/t1.part"
	orphan := tmp + "/stale.part"
	e.fs.AddFile(claimed, []byte("partial"), 0o600)
	e.fs.AddFile(orphan, []byte("stale"), 0o600)
	e.addTransfer(t, "t1", "big.iso", claimed)

	report := e.run(t, false)

	if len(report.OrphanTemps) != 1 || report.OrphanTemps[0] != orphan {
		t.Fatalf("orphans = %v, want [%s]", report.OrphanTemps, orphan)
	}
	if _, ok := e.fs.Content(orphan); ok {
		t.Fatal("orphan temp file should have been removed")
	}
	if _, ok := e.fs.Content(claimed); !ok {
		t.Fatal("the claimed temp file must stay")
	}
	if len(report.TransfersReset) != 0 {
		t.Fatalf("reset = %v, want none: the temp file exists", report.TransfersReset)
	}
}

func TestRepair_FixesFolderSequence(t *testing.T) {
	e := newEnv(t)
	e.addFile(t, "a.txt", "alpha")
	e.store.SetSequence(e.folder.ID, 0)

	if report := e.run(t, true); !report.SequenceFixed {
		t.Fatal("dry run should report the sequence as behind")
	}
	report := e.run(t, false)

	if !report.SequenceFixed {
		t.Fatal("repair should report the sequence as fixed")
	}
	if again := e.run(t, false); again.SequenceFixed {
		t.Fatal("the sequence should be consistent after the first repair")
	}
}

func TestRepair_DryRunNoWrites(t *testing.T) {
	e := newEnv(t)
	e.addFile(t, "a.txt", "alpha")
	e.addFile(t, "b.txt", "beta")
	if err := e.fs.Remove(root + "/a.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	e.fs.AddFile(root+"/b.txt", []byte("beta rewritten"), 0o644)
	orphan := root + "/" + repair.TempDir + "/stale.part"
	e.fs.AddFile(orphan, []byte("stale"), 0o600)

	report := e.run(t, true)

	if !report.DryRun || !report.Changed() {
		t.Fatalf("report = %+v, want a dry run with findings", report)
	}
	if got := e.live(t); len(got) != 2 {
		t.Fatalf("live entries = %v, want both still live", got)
	}
	if e.entry(t, "b.txt").Hash != e.hasher.SumBytes([]byte("beta")) {
		t.Fatal("the stored hash must not change during a dry run")
	}
	if _, ok := e.fs.Content(orphan); !ok {
		t.Fatal("the orphan temp file must survive a dry run")
	}
	if len(e.fs.Removes()) != 1 {
		t.Fatalf("filesystem removals = %v, want only the one the test made", e.fs.Removes())
	}
}

// compile-time check that the fake store still satisfies the port the service
// depends on.
var _ ports.MetadataStore = (*fakes.Store)(nil)

// removeAll deletes the given folder-relative paths, deepest first. An empty
// path is the root itself.
func removeAll(t *testing.T, e *env, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		full := root
		if rel != "" {
			full = root + "/" + rel
		}
		if err := e.fs.Remove(full); err != nil {
			t.Fatalf("remove %s: %v", full, err)
		}
	}
}

// TestRun_RefusesAMissingRoot proves repair does not read a folder whose root
// is gone as a folder whose files were all deleted: doing so would mark every
// entry deleted and send the deletions to the peers.
func TestRun_RefusesAMissingRoot(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addFile(t, "a.txt", "alpha")
	e.addFile(t, "sub/b.txt", "beta")

	removeAll(t, e, "sub/b.txt", "sub", "a.txt", "")

	if _, err := e.service.Run(ctx, "docs", false); !errors.Is(err, repair.ErrRootMissing) {
		t.Fatalf("repair = %v, want ErrRootMissing", err)
	}
	m, err := e.store.LoadManifest(ctx, "docs")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(m.Live()) != 2 {
		t.Fatalf("live entries = %d, want both of them kept", len(m.Live()))
	}
}

// TestRun_RefusesARootThatIsNoLongerADirectory covers the other half of the
// guard: a root replaced by a file.
func TestRun_RefusesARootThatIsNoLongerADirectory(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addFile(t, "a.txt", "alpha")
	removeAll(t, e, "a.txt", "")
	e.fs.AddFile(root, []byte("not a directory"), 0o644)

	if _, err := e.service.Run(ctx, "docs", false); !errors.Is(err, repair.ErrRootMissing) {
		t.Fatalf("repair = %v, want ErrRootMissing", err)
	}
}
