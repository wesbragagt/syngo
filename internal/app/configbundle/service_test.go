package configbundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/configbundle"
	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// peerID is a valid device id for the exported peer.
const peerID = device.ID("ETI3RRZAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

// at is the time the fake clock reads.
var at = time.Date(2026, time.September, 21, 8, 0, 0, 0, time.UTC)

// deviceSettings are the settings of a device that has been configured a
// little, so an import that writes them is a change.
func deviceSettings() ports.Settings {
	return ports.Settings{
		Listen: "127.0.0.1:22000", ChunkSize: transfer.DefaultChunkSize, MaxFileSize: 1 << 40,
		RescanInterval: 10 * time.Minute, SyncInterval: 30 * time.Second, LogLevel: "info",
		DownloadLimit: bandwidth.Rate(1 << 20),
	}
}

// node is one device's stores, as the bundle service sees them.
type node struct {
	store    *fakes.Store
	settings *fakes.Settings
	fs       *fakes.FileSystem
	svc      *configbundle.Service
}

// newNode returns an empty device whose filesystem holds the folder roots the
// tests use.
func newNode(t *testing.T, settings ports.Settings) *node {
	t.Helper()
	n := &node{store: fakes.NewStore(), settings: fakes.NewSettings(settings), fs: fakes.NewFileSystem()}
	n.fs.AddDir("/home/u/docs", 0o755)
	n.fs.AddDir("/home/u/notes", 0o755)
	n.svc = configbundle.New(n.store, n.store, n.settings, n.fs, fakes.NewClock(at))
	return n
}

// docsFolder returns a configured folder with everything a bundle carries.
func docsFolder(t *testing.T, id folder.ID, path string) folder.Folder {
	t.Helper()
	windows, err := folder.ParseWindows([]string{"22:00-06:00"})
	if err != nil {
		t.Fatalf("parse windows: %v", err)
	}
	return folder.Folder{
		ID: id, Path: path, Label: "Documents", Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkStore,
		IncludeHidden: true, KeepEmptyDirs: true, IgnorePatterns: []string{"*.tmp"},
		SyncWindows: windows, CreatedAt: at.Add(-time.Hour),
	}
}

// configured returns a device holding one folder and one peer.
func configured(t *testing.T) *node {
	t.Helper()
	ctx := context.Background()
	n := newNode(t, deviceSettings())
	if err := n.store.SaveFolder(ctx, docsFolder(t, "docs", "/home/u/docs")); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	err := n.store.SaveDevice(ctx, device.Device{
		ID: peerID, Name: "nas", Address: "10.0.0.2:22000", Verified: true,
		AddedAt: at.Add(-2 * time.Hour), LastSeenAt: at.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("save device: %v", err)
	}
	return n
}

func TestExport_ThenImportIntoEmptyStoresRoundTrips(t *testing.T) {
	ctx := context.Background()
	a := configured(t)
	bundle, err := a.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	b := newNode(t, ports.Settings{})
	result, err := b.svc.Import(ctx, bundle, configbundle.ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.FoldersAdded) != 1 || len(result.PeersAdded) != 1 || !result.SettingsChanged {
		t.Fatalf("result = %+v, want one folder, one peer and the settings", result)
	}

	again, err := b.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export of the importing device: %v", err)
	}
	bundle.ExportedAt, again.ExportedAt = "", ""
	if !equalJSON(t, bundle, again) {
		t.Fatalf("the round trip changed the configuration:\n%s\n%s",
			mustJSON(t, bundle), mustJSON(t, again))
	}
}

func TestExport_ContainsNoIdentityMaterial(t *testing.T) {
	bundle, err := configured(t).svc.Export(context.Background())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	raw := mustJSON(t, bundle)
	for _, forbidden := range []string{"device_name", "device_id", "identity", "\"key\"", "certificate"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("the bundle holds %s:\n%s", forbidden, raw)
		}
	}
}

func TestImport_ReportsEveryConflictAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	bundle, err := configured(t).svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// The bundle also names a second folder whose path this device already
	// synchronizes under another id.
	bundle.Folders = append(bundle.Folders, configbundle.Folder{
		ID: "notes", Path: "/home/u/notes", Mode: string(folder.ModeSendReceive),
		DeletePolicy: string(folder.DeletePropagate), Symlinks: string(folder.SymlinkSkip),
		IgnorePatterns: []string{}, SyncWindows: []string{},
	})

	// The receiving device already has docs, has /home/u/notes under the id
	// archive, and knows the peer.
	b := configured(t)
	other := docsFolder(t, "archive", "/home/u/notes")
	if err := b.store.SaveFolder(ctx, other); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	before, err := b.store.ListFolders(ctx)
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}

	_, err = b.svc.Import(ctx, bundle, configbundle.ImportOptions{})
	var conflicts *configbundle.ConflictError
	if !errors.As(err, &conflicts) {
		t.Fatalf("import error = %v, want a conflict error", err)
	}
	if !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("a conflict error does not match ErrAlreadyExists")
	}
	kinds := map[string]int{}
	for _, c := range conflicts.Conflicts {
		kinds[c.Kind]++
	}
	if len(conflicts.Conflicts) != 3 || kinds[configbundle.ConflictFolderID] != 1 ||
		kinds[configbundle.ConflictFolderPath] != 1 || kinds[configbundle.ConflictPeer] != 1 {
		t.Fatalf("conflicts = %+v, want one of each kind", conflicts.Conflicts)
	}

	after, err := b.store.ListFolders(ctx)
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d folders after a refused import, want %d", len(after), len(before))
	}
	if len(b.settings.Saved()) != 0 {
		t.Fatal("a refused import wrote the settings")
	}
}

func TestImport_ReplaceOverwritesAndKeepsTimestamps(t *testing.T) {
	ctx := context.Background()
	source := configured(t)
	bundle, err := source.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bundle.Folders[0].Mode = string(folder.ModeSendOnly)
	bundle.Peers[0].Name = "storage"

	b := configured(t)
	result, err := b.svc.Import(ctx, bundle, configbundle.ImportOptions{Replace: true})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.FoldersReplaced) != 1 || len(result.PeersReplaced) != 1 {
		t.Fatalf("result = %+v, want the folder and the peer replaced", result)
	}

	f, err := b.store.GetFolder(ctx, "docs")
	if err != nil {
		t.Fatalf("get folder: %v", err)
	}
	if f.Mode != folder.ModeSendOnly {
		t.Fatalf("mode = %q, want the one from the bundle", f.Mode)
	}
	if !f.CreatedAt.Equal(at.Add(-time.Hour)) {
		t.Fatalf("created_at = %s, want the one the device already had", f.CreatedAt)
	}
	d, err := b.store.GetDevice(ctx, peerID)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.Name != "storage" {
		t.Fatalf("peer name = %q, want the one from the bundle", d.Name)
	}
	if !d.AddedAt.Equal(at.Add(-2*time.Hour)) || !d.LastSeenAt.Equal(at.Add(-time.Minute)) {
		t.Fatalf("peer history = %+v, want the one the device already had", d)
	}
}

func TestImport_PathOwnedByAnotherIDConflictsEvenWithReplace(t *testing.T) {
	ctx := context.Background()
	bundle, err := configured(t).svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	b := newNode(t, deviceSettings())
	// The same directory, under another id.
	if err := b.store.SaveFolder(ctx, docsFolder(t, "archive", "/home/u/docs")); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	_, err = b.svc.Import(ctx, bundle, configbundle.ImportOptions{Replace: true})
	var conflicts *configbundle.ConflictError
	if !errors.As(err, &conflicts) {
		t.Fatalf("import error = %v, want a conflict error", err)
	}
	if len(conflicts.Conflicts) != 1 || conflicts.Conflicts[0].Kind != configbundle.ConflictFolderPath {
		t.Fatalf("conflicts = %+v, want the path conflict", conflicts.Conflicts)
	}
}

func TestImport_DryRunWritesNothingAndReportsChanges(t *testing.T) {
	ctx := context.Background()
	bundle, err := configured(t).svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	b := newNode(t, ports.Settings{})
	result, err := b.svc.Import(ctx, bundle, configbundle.ImportOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !result.DryRun || !result.Changed() || len(result.FoldersAdded) != 1 {
		t.Fatalf("result = %+v, want a dry run that would add the folder", result)
	}
	folders, err := b.store.ListFolders(ctx)
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}
	if len(folders) != 0 || len(b.settings.Saved()) != 0 {
		t.Fatalf("the dry run wrote %d folders and %d settings", len(folders), len(b.settings.Saved()))
	}
}

func TestImport_FolderPathNotADirectoryIsInvalid(t *testing.T) {
	ctx := context.Background()
	bundle, err := configured(t).svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	b := newNode(t, deviceSettings())
	b.fs.AddFile("/home/u/file", []byte("hi"), 0o644)
	bundle.Folders[0].Path = "/home/u/file"
	if _, err := b.svc.Import(ctx, bundle, configbundle.ImportOptions{}); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("import error = %v, want ErrInvalidBundle", err)
	}

	bundle.Folders[0].Path = "/home/u/gone"
	if _, err := b.svc.Import(ctx, bundle, configbundle.ImportOptions{}); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("import of a missing path = %v, want ErrInvalidBundle", err)
	}
}

func TestImport_BadWindowOrPatternIsInvalid(t *testing.T) {
	ctx := context.Background()
	source := configured(t)
	b := newNode(t, deviceSettings())

	bad, err := source.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bad.Folders[0].SyncWindows = []string{"22:00-22:00"}
	if _, err := b.svc.Import(ctx, bad, configbundle.ImportOptions{}); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("import with a bad window = %v, want ErrInvalidBundle", err)
	}

	bad, err = source.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bad.Folders[0].Mode = "sideways"
	if _, err := b.svc.Import(ctx, bad, configbundle.ImportOptions{}); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("import with a bad mode = %v, want ErrInvalidBundle", err)
	}

	bad, err = source.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bad.Peers[0].Address = ""
	if _, err := b.svc.Import(ctx, bad, configbundle.ImportOptions{}); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("import of a peer without an address = %v, want ErrInvalidBundle", err)
	}

	bad, err = source.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bad.Settings.ChunkSize = transfer.MinChunkSize - 1
	if _, err := b.svc.Import(ctx, bad, configbundle.ImportOptions{}); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("import with a tiny chunk size = %v, want ErrInvalidBundle", err)
	}
}

func TestDecode_RejectsUnknownFieldAndOtherSchemaVersion(t *testing.T) {
	if _, err := configbundle.Decode(strings.NewReader(
		`{"schema_version":1,"nonsense":true}`)); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("decode with an unknown field = %v, want ErrInvalidBundle", err)
	}
	if _, err := configbundle.Decode(strings.NewReader(
		`{"schema_version":2}`)); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("decode of another schema version = %v, want ErrInvalidBundle", err)
	}
	if _, err := configbundle.Decode(strings.NewReader(`not json`)); !errors.Is(err, configbundle.ErrInvalidBundle) {
		t.Fatalf("decode of a non-document = %v, want ErrInvalidBundle", err)
	}
}

func TestImport_SettingsUnchangedIsNotReported(t *testing.T) {
	ctx := context.Background()
	bundle, err := configured(t).svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	bundle.Folders = nil
	bundle.Peers = nil

	// The receiving device already holds exactly these settings.
	b := newNode(t, deviceSettings())
	result, err := b.svc.Import(ctx, bundle, configbundle.ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.SettingsChanged || result.Changed() {
		t.Fatalf("result = %+v, want nothing reported", result)
	}
	if len(b.settings.Saved()) != 0 {
		t.Fatal("settings that did not change were written anyway")
	}
}

// mustJSON renders a value as indented JSON.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// equalJSON reports whether two values render to the same JSON.
func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	return mustJSON(t, a) == mustJSON(t, b)
}

func TestImport_ReplaceOfAnIdenticalConfigurationIsNotReported(t *testing.T) {
	ctx := context.Background()
	a := configured(t)
	bundle, err := a.svc.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// The same device the bundle came from: every item is already exactly
	// as the bundle describes it, so nothing is written.
	result, err := a.svc.Import(ctx, bundle, configbundle.ImportOptions{Replace: true})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Changed() {
		t.Fatalf("result = %+v, want nothing reported for an unchanged configuration", result)
	}
}
