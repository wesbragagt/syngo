package conflicts_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/conflicts"
	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// root is the folder every test resolves conflicts in.
const root = "/home/u/docs"

// fixture is a service with one folder, one conflict and both versions on disk.
type fixture struct {
	store   *fakes.Store
	fs      *fakes.FileSystem
	service *conflicts.Service
	record  ports.ConflictRecord
	local   []byte
	remote  []byte
}

// newFixture builds a both-modified conflict: the peer's version at the path
// and the local one beside it, which is what the puller leaves behind.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	hasher := fakes.NewHasher()
	f := &fixture{
		store: fakes.NewStore(), fs: fakes.NewFileSystem(),
		local: []byte("what this device wrote"), remote: []byte("what the peer wrote"),
	}
	f.service = conflicts.New(f.store, f.fs, hasher)

	ctx := context.Background()
	err := f.store.SaveFolder(ctx, folder.Folder{
		ID: "docs", Path: root, Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkStore,
	})
	if err != nil {
		t.Fatalf("save folder: %v", err)
	}

	copyPath := "notes/a.sync-conflict-20260921-153000-ABCDEFG.txt"
	f.fs.AddDir(root+"/notes", 0o755)
	f.fs.AddFile(root+"/notes/a.txt", f.remote, 0o644)
	f.fs.AddFile(root+"/"+copyPath, f.local, 0o644)

	f.record = ports.ConflictRecord{
		ID: "c1", FolderID: "docs", Path: "notes/a.txt", ConflictPath: copyPath,
		LocalHash: hasher.SumBytes(f.local), RemoteHash: hasher.SumBytes(f.remote),
		PeerID:     "PEERDEVICE",
		DetectedAt: time.Date(2026, time.September, 21, 15, 30, 0, 0, time.UTC),
		Reason:     "both-modified",
	}
	if err := f.store.SaveConflict(ctx, f.record); err != nil {
		t.Fatalf("save conflict: %v", err)
	}
	return f
}

// content returns what a folder-relative path holds.
func (f *fixture) content(t *testing.T, rel string) string {
	t.Helper()
	data, ok := f.fs.Content(root + "/" + rel)
	if !ok {
		t.Fatalf("%s does not exist", rel)
	}
	return string(data)
}

// gone reports whether a folder-relative path is absent.
func (f *fixture) gone(rel string) bool {
	_, ok := f.fs.Content(root + "/" + rel)
	return !ok
}

func TestList_LeavesOutResolvedOnesUnlessAsked(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	list, err := f.service.List(ctx, "docs", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != "c1" || list[0].FolderPath != root {
		t.Fatalf("list = %+v, want the open conflict with its folder path", list)
	}

	if _, err := f.service.Resolve(ctx, "c1", conflicts.KeepBoth); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if list, err = f.service.List(ctx, "", false); err != nil || len(list) != 0 {
		t.Fatalf("list = %+v (%v), want nothing after it was resolved", list, err)
	}
	if list, err = f.service.List(ctx, "", true); err != nil || len(list) != 1 {
		t.Fatalf("list --all = %+v (%v), want the resolved conflict", list, err)
	}
}

func TestList_UnknownFolderIsNotAnEmptyList(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.List(context.Background(), "gone", false); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("list of an unknown folder = %v, want ErrNotFound", err)
	}
}

func TestShow_DescribesBothVersions(t *testing.T) {
	f := newFixture(t)
	view, err := f.service.Show(context.Background(), "c1")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if view.File == nil || view.Copy == nil {
		t.Fatalf("view = %+v, want both files described", view)
	}
	if view.File.Hash != view.RemoteHash {
		t.Fatalf("the file at the path holds %s, want the peer's version %s",
			view.File.Hash, view.RemoteHash)
	}
	if view.Copy.Hash != view.LocalHash {
		t.Fatalf("the copy holds %s, want the local version %s", view.Copy.Hash, view.LocalHash)
	}
	if view.File.Size != int64(len(f.remote)) || view.Copy.Size != int64(len(f.local)) {
		t.Fatalf("sizes = %d and %d, want %d and %d",
			view.File.Size, view.Copy.Size, len(f.remote), len(f.local))
	}
	if view.File.ModTime.IsZero() {
		t.Fatal("no modification time was reported")
	}
	// Which file holds which version is answered by content, so a version
	// is found wherever it sits.
	if holder := view.Holder(view.LocalHash); holder == nil || holder.Path != view.ConflictPath {
		t.Fatalf("the local version is reported in %+v, want the copy", holder)
	}
	if holder := view.Holder(view.RemoteHash); holder == nil || holder.Path != view.Path {
		t.Fatalf("the remote version is reported in %+v, want the path", holder)
	}
}

func TestShow_UnknownIDIsNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Show(context.Background(), "nope"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("show of an unknown id = %v, want ErrNotFound", err)
	}
}

func TestResolve_KeepLocalMovesTheCopyBack(t *testing.T) {
	f := newFixture(t)
	result, err := f.service.Resolve(context.Background(), "c1", conflicts.KeepLocal)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := f.content(t, "notes/a.txt"); got != string(f.local) {
		t.Fatalf("notes/a.txt = %q, want the local version", got)
	}
	if !f.gone(f.record.ConflictPath) {
		t.Fatal("the conflict copy is still there")
	}
	if result.KeptPath != "notes/a.txt" || result.MovedFrom != f.record.ConflictPath {
		t.Fatalf("result = %+v, want the copy moved onto the path", result)
	}
	if !result.View.Resolved {
		t.Fatal("the conflict was not marked resolved")
	}
}

func TestResolve_KeepRemoteRemovesTheCopy(t *testing.T) {
	f := newFixture(t)
	result, err := f.service.Resolve(context.Background(), "c1", conflicts.KeepRemote)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := f.content(t, "notes/a.txt"); got != string(f.remote) {
		t.Fatalf("notes/a.txt = %q, want the peer's version", got)
	}
	if !f.gone(f.record.ConflictPath) {
		t.Fatal("the conflict copy is still there")
	}
	if result.RemovedPath != f.record.ConflictPath || result.MovedFrom != "" {
		t.Fatalf("result = %+v, want only the copy removed", result)
	}
}

func TestResolve_KeepBothTouchesNoFile(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Resolve(context.Background(), "c1", conflicts.KeepBoth); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := f.content(t, "notes/a.txt"); got != string(f.remote) {
		t.Fatalf("notes/a.txt = %q, want it untouched", got)
	}
	if got := f.content(t, f.record.ConflictPath); got != string(f.local) {
		t.Fatalf("the copy = %q, want it untouched", got)
	}
	if len(f.fs.Removes()) != 0 || len(f.fs.Renames()) != 0 {
		t.Fatalf("removes = %v, renames = %v, want none", f.fs.Removes(), f.fs.Renames())
	}
}

func TestResolve_VersionThatIsGoneIsRefused(t *testing.T) {
	f := newFixture(t)
	// Both files are edited after the conflict was recorded, so neither
	// holds what the user asks for.
	f.fs.AddFile(root+"/notes/a.txt", []byte("edited since"), 0o644)
	f.fs.AddFile(root+"/"+f.record.ConflictPath, []byte("edited too"), 0o644)

	_, err := f.service.Resolve(context.Background(), "c1", conflicts.KeepLocal)
	if !errors.Is(err, conflicts.ErrVersionGone) {
		t.Fatalf("resolve = %v, want ErrVersionGone", err)
	}
	if got := f.content(t, "notes/a.txt"); got != "edited since" {
		t.Fatalf("notes/a.txt = %q, want it untouched by the refusal", got)
	}
	list, err := f.service.List(context.Background(), "docs", false)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v (%v), want the conflict still open", list, err)
	}
}

func TestResolve_KeepRemoteOnADeletionRemovesTheFile(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	// A delete-vs-modify conflict: the peer deleted the path, so there is
	// no copy and no remote hash.
	record := f.record
	record.ID = "c2"
	record.Path = "notes/deleted.txt"
	record.ConflictPath = ""
	record.RemoteHash = manifest.Hash{}
	record.LocalHash = fakes.NewHasher().SumBytes([]byte("kept here"))
	record.Reason = "delete-vs-modify"
	f.fs.AddFile(root+"/notes/deleted.txt", []byte("kept here"), 0o644)
	if err := f.store.SaveConflict(ctx, record); err != nil {
		t.Fatalf("save conflict: %v", err)
	}

	result, err := f.service.Resolve(ctx, "c2", conflicts.KeepRemote)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !f.gone("notes/deleted.txt") {
		t.Fatal("the file is still there after following the peer's deletion")
	}
	if result.RemovedPath != "notes/deleted.txt" || result.KeptPath != "" {
		t.Fatalf("result = %+v, want the file removed", result)
	}

	// Keeping the local version of the same conflict changes nothing.
	f.fs.AddFile(root+"/notes/deleted.txt", []byte("kept here"), 0o644)
	if err := f.store.SetConflictResolved(ctx, "c2", false); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := f.service.Resolve(ctx, "c2", conflicts.KeepLocal); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if f.gone("notes/deleted.txt") {
		t.Fatal("keeping the local version removed the file")
	}
}

func TestExport_WritesBothVersionsAndADescription(t *testing.T) {
	f := newFixture(t)
	result, err := f.service.Export(context.Background(), "c1", "/tmp/review")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(result.Files) != 2 {
		t.Fatalf("files = %v, want both versions", result.Files)
	}
	for _, name := range result.Files {
		data, ok := f.fs.Content("/tmp/review/" + name)
		if !ok {
			t.Fatalf("%s was not written", name)
		}
		if string(data) != string(f.local) && string(data) != string(f.remote) {
			t.Fatalf("%s = %q, want one of the two versions", name, data)
		}
	}

	raw, ok := f.fs.Content("/tmp/review/" + result.Sidecar)
	if !ok {
		t.Fatalf("%s was not written", result.Sidecar)
	}
	var doc struct {
		ID     string `json:"id"`
		Path   string `json:"path"`
		Reason string `json:"reason"`
		Files  []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Hash    string `json:"hash"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("sidecar is not json: %v\n%s", err, raw)
	}
	if doc.ID != "c1" || doc.Path != "notes/a.txt" || doc.Reason != "both-modified" {
		t.Fatalf("sidecar = %+v, want it to describe the conflict", doc)
	}
	versions := map[string]string{}
	for _, file := range doc.Files {
		versions[file.Version] = file.Name
	}
	if versions["local"] == "" || versions["remote"] == "" {
		t.Fatalf("sidecar files = %+v, want both versions named", doc.Files)
	}

	// Exporting changes nothing in the folder.
	if got := f.content(t, "notes/a.txt"); got != string(f.remote) {
		t.Fatalf("notes/a.txt = %q, want it untouched", got)
	}
	list, err := f.service.List(context.Background(), "docs", false)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v (%v), want the conflict still open", list, err)
	}
}

func TestResolve_KeepLocalRefusesToOverwriteAChangedPath(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	// The path moved on: a later round of edits put content there that
	// neither side had when this conflict was recorded. Renaming the copy
	// over it would throw that content away.
	f.fs.AddFile(root+"/notes/a.txt", []byte("a later round wrote this"), 0o644)

	_, err := f.service.Resolve(ctx, "c1", conflicts.KeepLocal)
	if !errors.Is(err, conflicts.ErrChanged) {
		t.Fatalf("resolve = %v, want ErrChanged", err)
	}
	if !strings.Contains(err.Error(), "notes/a.txt") {
		t.Fatalf("error = %q, want it to name the file that changed", err)
	}
	if got := f.content(t, "notes/a.txt"); got != "a later round wrote this" {
		t.Fatalf("notes/a.txt = %q, want it untouched by the refusal", got)
	}
	if got := f.content(t, f.record.ConflictPath); got != string(f.local) {
		t.Fatalf("the copy = %q, want it untouched by the refusal", got)
	}
	if len(f.fs.Removes()) != 0 || len(f.fs.Renames()) != 0 {
		t.Fatalf("removes = %v, renames = %v, want none", f.fs.Removes(), f.fs.Renames())
	}
	list, err := f.service.List(ctx, "docs", false)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v (%v), want the conflict still open", list, err)
	}
}

func TestResolve_KeepRemoteRefusesToDeleteAChangedCopy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	// The copy was edited by hand, so it holds neither version. Removing
	// it would be the only copy of that content gone.
	f.fs.AddFile(root+"/"+f.record.ConflictPath, []byte("merged by hand"), 0o644)

	_, err := f.service.Resolve(ctx, "c1", conflicts.KeepRemote)
	if !errors.Is(err, conflicts.ErrChanged) {
		t.Fatalf("resolve = %v, want ErrChanged", err)
	}
	if !strings.Contains(err.Error(), f.record.ConflictPath) {
		t.Fatalf("error = %q, want it to name the copy that changed", err)
	}
	if got := f.content(t, f.record.ConflictPath); got != "merged by hand" {
		t.Fatalf("the copy = %q, want it untouched by the refusal", got)
	}
	if got := f.content(t, "notes/a.txt"); got != string(f.remote) {
		t.Fatalf("notes/a.txt = %q, want it untouched by the refusal", got)
	}
	if len(f.fs.Removes()) != 0 || len(f.fs.Renames()) != 0 {
		t.Fatalf("removes = %v, renames = %v, want none", f.fs.Removes(), f.fs.Renames())
	}
}

func TestResolve_FollowingADeletionRefusesAChangedFile(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	record := f.record
	record.ID = "c2"
	record.Path = "notes/deleted.txt"
	record.ConflictPath = ""
	record.RemoteHash = manifest.Hash{}
	record.LocalHash = fakes.NewHasher().SumBytes([]byte("kept here"))
	record.Reason = "delete-vs-modify"
	f.fs.AddFile(root+"/notes/deleted.txt", []byte("edited again since"), 0o644)
	if err := f.store.SaveConflict(ctx, record); err != nil {
		t.Fatalf("save conflict: %v", err)
	}

	_, err := f.service.Resolve(ctx, "c2", conflicts.KeepRemote)
	if !errors.Is(err, conflicts.ErrChanged) {
		t.Fatalf("resolve = %v, want ErrChanged", err)
	}
	if got := f.content(t, "notes/deleted.txt"); got != "edited again since" {
		t.Fatalf("notes/deleted.txt = %q, want it untouched by the refusal", got)
	}
}

func TestResolve_AnAlreadyResolvedConflictIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.service.Resolve(ctx, "c1", conflicts.KeepLocal); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The copy is gone and the path holds the local version. Resolving
	// again must not act on whatever sits there now.
	_, err := f.service.Resolve(ctx, "c1", conflicts.KeepRemote)
	if !errors.Is(err, conflicts.ErrAlreadyResolved) {
		t.Fatalf("resolving twice = %v, want ErrAlreadyResolved", err)
	}
	if !strings.Contains(err.Error(), `"c1"`) {
		t.Fatalf("error = %q, want it to name the conflict", err)
	}
	if got := f.content(t, "notes/a.txt"); got != string(f.local) {
		t.Fatalf("notes/a.txt = %q, want the first resolution left alone", got)
	}
}

func TestListAndShow_MarkAConflictNothingCanDecideAsStale(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	list, err := f.service.List(ctx, "docs", false)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v (%v), want the open conflict", list, err)
	}
	if list[0].Stale {
		t.Fatal("a conflict whose two versions are both on disk is reported stale")
	}
	if list[0].File != nil || list[0].Copy != nil {
		t.Fatalf("list = %+v, want the files left to show", list[0])
	}

	// A later round of edits puts content at the path that neither side
	// had, so neither version can be kept any more.
	f.fs.AddFile(root+"/notes/a.txt", []byte("a later round wrote this"), 0o644)
	if list, err = f.service.List(ctx, "docs", false); err != nil || len(list) != 1 || !list[0].Stale {
		t.Fatalf("list = %+v (%v), want the conflict marked stale", list, err)
	}
	view, err := f.service.Show(ctx, "c1")
	if err != nil || !view.Stale {
		t.Fatalf("show = %+v (%v), want the conflict marked stale", view, err)
	}

	// Keeping both is what closes it, and it touches no file.
	if _, err := f.service.Resolve(ctx, "c1", conflicts.KeepBoth); err != nil {
		t.Fatalf("resolve --keep both: %v", err)
	}
	if got := f.content(t, "notes/a.txt"); got != "a later round wrote this" {
		t.Fatalf("notes/a.txt = %q, want it untouched", got)
	}
	if list, err = f.service.List(ctx, "docs", false); err != nil || len(list) != 0 {
		t.Fatalf("list = %+v (%v), want the conflict closed", list, err)
	}
}

func TestExport_KeepsTheModeOfEachVersion(t *testing.T) {
	f := newFixture(t)
	f.fs.AddFile(root+"/notes/a.txt", f.remote, 0o755)
	f.fs.AddFile(root+"/"+f.record.ConflictPath, f.local, 0o640)

	result, err := f.service.Export(context.Background(), "c1", "/tmp/review")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := map[string]fs.FileMode{"path-a.txt": 0o755}
	want["conflict-"+path.Base(f.record.ConflictPath)] = 0o640
	for _, name := range result.Files {
		info, err := f.fs.Lstat("/tmp/review/" + name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode.Perm() != want[name] {
			t.Fatalf("%s has mode %o, want %o", name, info.Mode.Perm(), want[name])
		}
	}
}

// TestList_LeavesOutARecordWhoseFolderIsGone proves a listing never describes
// files through an empty folder path: with the zero value every path in the
// view would be resolved from the working directory instead.
func TestList_LeavesOutARecordWhoseFolderIsGone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	other := ports.ConflictRecord{
		ID: "c2", FolderID: "photos", Path: "b.txt",
		DetectedAt: f.record.DetectedAt, Reason: "both-modified",
	}
	err := f.store.SaveFolder(ctx, folder.Folder{
		ID: "photos", Path: "/home/u/photos", Mode: folder.ModeSendReceive,
	})
	if err != nil {
		t.Fatalf("save folder: %v", err)
	}
	if err := f.store.SaveConflict(ctx, other); err != nil {
		t.Fatalf("save conflict: %v", err)
	}
	if err := f.store.DeleteFolder(ctx, "docs"); err != nil {
		t.Fatalf("delete folder: %v", err)
	}

	views, err := f.service.List(ctx, "", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 1 || views[0].ID != "c2" {
		t.Fatalf("views = %+v, want only the conflict whose folder still exists", views)
	}
	if views[0].FolderPath != "/home/u/photos" {
		t.Fatalf("folder path = %q", views[0].FolderPath)
	}
}
