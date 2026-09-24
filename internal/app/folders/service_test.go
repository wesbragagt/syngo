package folders_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/folders"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// newService returns a service backed by fakes, with /docs and /music present
// on the fake filesystem.
func newService(t *testing.T) (*folders.Service, *fakes.Store, *fakes.Clock) {
	t.Helper()
	fs := fakes.NewFileSystem()
	fs.AddDir("/docs", 0o755)
	fs.AddDir("/music", 0o755)
	fs.AddFile("/notes.txt", []byte("hi"), 0o644)
	store := fakes.NewStore()
	clock := fakes.NewClock(time.Time{})
	return folders.New(store, store, fs, clock), store, clock
}

// docsFolder returns a valid folder definition.
func docsFolder(id folder.ID, path string) folder.Folder {
	return folder.Folder{
		ID: id, Path: path, Mode: folder.ModeSendReceive, DeletePolicy: folder.DeletePropagate,
		Symlinks: folder.SymlinkSkip, KeepEmptyDirs: true,
	}
}

func TestAdd_StoresWithCreationTime(t *testing.T) {
	svc, store, clock := newService(t)
	ctx := context.Background()

	got, err := svc.Add(ctx, docsFolder("docs", "/docs"))
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if !got.CreatedAt.Equal(clock.Now()) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, clock.Now())
	}

	saved, err := store.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("GetFolder error: %v", err)
	}
	if saved.Path != "/docs" {
		t.Fatalf("saved folder = %+v", saved)
	}
}

func TestAdd_RejectsDuplicateID(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Add(ctx, docsFolder("docs", "/docs")); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	_, err := svc.Add(ctx, docsFolder("docs", "/music"))
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("Add error = %v, want ErrAlreadyExists", err)
	}
}

func TestAdd_RejectsDuplicatePath(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Add(ctx, docsFolder("docs", "/docs")); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	_, err := svc.Add(ctx, docsFolder("other", "/docs"))
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("Add error = %v, want ErrAlreadyExists", err)
	}
}

func TestAdd_RequiresExistingDir(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	tests := []struct {
		name string
		path string
	}{
		{"missing", "/nope"},
		{"not a directory", "/notes.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.Add(ctx, docsFolder("docs", tt.path)); err == nil {
				t.Fatal("Add error = nil, want a failure")
			}
		})
	}
}

func TestAdd_RejectsInvalidFolder(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	bad := docsFolder("Docs", "/docs")
	if _, err := svc.Add(ctx, bad); !errors.Is(err, folder.ErrInvalidID) {
		t.Fatalf("Add error = %v, want ErrInvalidID", err)
	}
}

func TestRemove_NotFound(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	if err := svc.Remove(ctx, "docs"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Remove error = %v, want ErrNotFound", err)
	}

	if _, err := svc.Add(ctx, docsFolder("docs", "/docs")); err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if err := svc.Remove(ctx, "docs"); err != nil {
		t.Fatalf("Remove error: %v", err)
	}
	if _, err := svc.Get(ctx, "docs"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get after remove = %v, want ErrNotFound", err)
	}
}

func TestListAndSetPaused(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	for _, f := range []folder.Folder{docsFolder("music", "/music"), docsFolder("docs", "/docs")} {
		if _, err := svc.Add(ctx, f); err != nil {
			t.Fatalf("Add error: %v", err)
		}
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(list) != 2 || list[0].ID != "docs" || list[1].ID != "music" {
		t.Fatalf("List = %+v, want docs then music", list)
	}

	if err := svc.SetPaused(ctx, "docs", true); err != nil {
		t.Fatalf("SetPaused error: %v", err)
	}
	got, err := svc.Get(ctx, "docs")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if !got.Paused {
		t.Fatal("folder is not paused")
	}
	if err := svc.SetPaused(ctx, "nope", true); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("SetPaused on unknown folder = %v, want ErrNotFound", err)
	}
}

func TestUpdate_ChangesOnlyWhatIsGiven(t *testing.T) {
	svc, store, _ := newService(t)
	added, err := svc.Add(context.Background(), docsFolder("docs", "/docs"))
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	mode := folder.ModeReceiveOnly
	updated, err := svc.Update(context.Background(), added.ID, folders.Changes{Mode: &mode})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Mode != folder.ModeReceiveOnly {
		t.Fatalf("mode = %q, want receiveonly", updated.Mode)
	}
	if updated.DeletePolicy != added.DeletePolicy || updated.Path != added.Path {
		t.Fatalf("update changed more than the mode: %+v", updated)
	}

	stored, err := store.GetFolder(context.Background(), added.ID)
	if err != nil {
		t.Fatalf("get folder: %v", err)
	}
	if stored.Mode != folder.ModeReceiveOnly {
		t.Fatalf("stored mode = %q, want receiveonly", stored.Mode)
	}
}

func TestUpdate_RejectsAnInvalidPolicyAndKeepsTheFolder(t *testing.T) {
	svc, store, _ := newService(t)
	added, err := svc.Add(context.Background(), docsFolder("docs", "/docs"))
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	policy := folder.DeletePolicy("sometimes")
	if _, err := svc.Update(context.Background(), added.ID, folders.Changes{DeletePolicy: &policy}); err == nil {
		t.Fatal("update accepted an invalid delete policy")
	}
	stored, err := store.GetFolder(context.Background(), added.ID)
	if err != nil {
		t.Fatalf("get folder: %v", err)
	}
	if stored.DeletePolicy != added.DeletePolicy {
		t.Fatalf("delete policy = %q, want the original %q", stored.DeletePolicy, added.DeletePolicy)
	}
}

func TestUpdate_UnknownFolder(t *testing.T) {
	svc, _, _ := newService(t)

	mode := folder.ModeSendOnly
	if _, err := svc.Update(context.Background(), "nope", folders.Changes{Mode: &mode}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("update unknown = %v, want ErrNotFound", err)
	}
}

func TestUpdate_SyncWindowsReplaceAndClear(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newService(t)
	added, err := svc.Add(ctx, docsFolder("docs", "/docs"))
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if !added.SyncWindows.Always() {
		t.Fatalf("a new folder has windows %v, want none", added.SyncWindows.Strings())
	}

	nightly, err := folder.ParseWindows([]string{"22:00-06:00"})
	if err != nil {
		t.Fatalf("parse window: %v", err)
	}
	updated, err := svc.Update(ctx, "docs", folders.Changes{SyncWindows: &nightly})
	if err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if folder.EncodeWindows(updated.SyncWindows) != "22:00-06:00" {
		t.Fatalf("windows = %v, want the one that was set", updated.SyncWindows.Strings())
	}

	// A second list replaces the first rather than adding to it.
	noon, err := folder.ParseWindows([]string{"12:00-13:00"})
	if err != nil {
		t.Fatalf("parse window: %v", err)
	}
	if updated, err = svc.Update(ctx, "docs", folders.Changes{SyncWindows: &noon}); err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if folder.EncodeWindows(updated.SyncWindows) != "12:00-13:00" {
		t.Fatalf("windows = %v, want only the second list", updated.SyncWindows.Strings())
	}

	cleared := folder.Windows{}
	if updated, err = svc.Update(ctx, "docs", folders.Changes{SyncWindows: &cleared}); err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if !updated.SyncWindows.Always() {
		t.Fatalf("windows = %v, want them cleared", updated.SyncWindows.Strings())
	}
	stored, err := store.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("GetFolder error: %v", err)
	}
	if !stored.SyncWindows.Always() {
		t.Fatalf("stored windows = %v, want them cleared", stored.SyncWindows.Strings())
	}
}
