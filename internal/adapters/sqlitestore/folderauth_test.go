package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syngo/internal/adapters/sqlitestore"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

const (
	storeDevA = device.ID("ETI3RRZAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	storeDevB = device.ID("ETI3RRZBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
)

// authStore returns a store holding two folders and two paired devices, with
// nothing authorized yet.
func authStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	ctx := context.Background()
	store, _ := openStore(t)
	for _, f := range []folder.Folder{testFolder("docs", "/home/u/docs"), testFolder("pics", "/home/u/pics")} {
		if err := store.SaveFolder(ctx, f); err != nil {
			t.Fatalf("SaveFolder %s: %v", f.ID, err)
		}
	}
	for _, id := range []device.ID{storeDevA, storeDevB} {
		err := store.SaveDevice(ctx, device.Device{ID: id, Name: "peer", AddedAt: testTime})
		if err != nil {
			t.Fatalf("SaveDevice %s: %v", id.Short(), err)
		}
	}
	return store
}

func TestFolderAccess_IsEmptyUntilSomethingIsGranted(t *testing.T) {
	ctx := context.Background()
	store := authStore(t)

	access, err := store.FolderAccess(ctx, "docs")
	if err != nil {
		t.Fatalf("FolderAccess: %v", err)
	}
	if !access.Empty() || access.Allows(storeDevA) {
		t.Fatalf("a folder nobody was granted allows %v", access.Devices())
	}

	// A folder that does not exist is empty rather than an error: a caller
	// asking on behalf of a peer must not be able to tell the two apart.
	access, err = store.FolderAccess(ctx, "nope")
	if err != nil {
		t.Fatalf("FolderAccess of an unknown folder: %v", err)
	}
	if !access.Empty() {
		t.Fatalf("an unknown folder allows %v", access.Devices())
	}
}

func TestGrantFolderDevice_IsPerFolderAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := authStore(t)

	if err := store.GrantFolderDevice(ctx, "docs", storeDevA); err != nil {
		t.Fatalf("GrantFolderDevice: %v", err)
	}
	if err := store.GrantFolderDevice(ctx, "docs", storeDevA); err != nil {
		t.Fatalf("second GrantFolderDevice: %v", err)
	}

	access, err := store.FolderAccess(ctx, "docs")
	if err != nil {
		t.Fatalf("FolderAccess: %v", err)
	}
	if got := access.Devices(); len(got) != 1 || got[0] != storeDevA {
		t.Fatalf("docs allows %v, want [%s]", got, storeDevA)
	}
	// The grant says nothing about the other folder or the other device.
	if access.Allows(storeDevB) {
		t.Error("granting one device authorized another")
	}
	other, err := store.FolderAccess(ctx, "pics")
	if err != nil {
		t.Fatalf("FolderAccess of pics: %v", err)
	}
	if !other.Empty() {
		t.Errorf("granting docs authorized pics for %v", other.Devices())
	}
}

func TestGrantFolderDevice_RefusesWhatIsNotConfigured(t *testing.T) {
	ctx := context.Background()
	store := authStore(t)

	if err := store.GrantFolderDevice(ctx, "nope", storeDevA); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("grant on an unknown folder = %v, want %v", err, ports.ErrNotFound)
	}
	unpaired := device.ID("ETI3RRZCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
	if err := store.GrantFolderDevice(ctx, "docs", unpaired); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("grant to an unpaired device = %v, want %v", err, ports.ErrNotFound)
	}
}

func TestRevokeFolderDevice_RemovesOneGrantAndReportsAMissingOne(t *testing.T) {
	ctx := context.Background()
	store := authStore(t)
	for _, id := range []folder.ID{"docs", "pics"} {
		if err := store.GrantFolderDevice(ctx, id, storeDevA); err != nil {
			t.Fatalf("GrantFolderDevice %s: %v", id, err)
		}
	}

	if err := store.RevokeFolderDevice(ctx, "docs", storeDevA); err != nil {
		t.Fatalf("RevokeFolderDevice: %v", err)
	}
	access, err := store.FolderAccess(ctx, "docs")
	if err != nil {
		t.Fatalf("FolderAccess: %v", err)
	}
	if !access.Empty() {
		t.Fatalf("docs still allows %v", access.Devices())
	}
	folders, err := store.DeviceFolders(ctx, storeDevA)
	if err != nil {
		t.Fatalf("DeviceFolders: %v", err)
	}
	if len(folders) != 1 || folders[0] != "pics" {
		t.Fatalf("device folders = %v, want [pics]", folders)
	}

	// Revoking twice names a grant that is not there.
	if err := store.RevokeFolderDevice(ctx, "docs", storeDevA); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("second revoke = %v, want %v", err, ports.ErrNotFound)
	}
}

func TestDeviceFolders_IsSortedAndPerDevice(t *testing.T) {
	ctx := context.Background()
	store := authStore(t)
	if err := store.GrantFolderDevice(ctx, "pics", storeDevA); err != nil {
		t.Fatalf("GrantFolderDevice pics: %v", err)
	}
	if err := store.GrantFolderDevice(ctx, "docs", storeDevA); err != nil {
		t.Fatalf("GrantFolderDevice docs: %v", err)
	}

	got, err := store.DeviceFolders(ctx, storeDevA)
	if err != nil {
		t.Fatalf("DeviceFolders: %v", err)
	}
	if len(got) != 2 || got[0] != "docs" || got[1] != "pics" {
		t.Fatalf("device folders = %v, want [docs pics]", got)
	}
	other, err := store.DeviceFolders(ctx, storeDevB)
	if err != nil {
		t.Fatalf("DeviceFolders of the other device: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("the device that was granted nothing has %v", other)
	}
}

// TestDeleteFolder_TakesItsAuthorizationsWithIt and its device counterpart
// pin the cascade: a grant that outlived what it names would authorize the
// next folder or device to reuse the id.
func TestDeleteFolder_TakesItsAuthorizationsWithIt(t *testing.T) {
	ctx := context.Background()
	store := authStore(t)
	if err := store.GrantFolderDevice(ctx, "docs", storeDevA); err != nil {
		t.Fatalf("GrantFolderDevice: %v", err)
	}

	if err := store.DeleteFolder(ctx, "docs"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	got, err := store.DeviceFolders(ctx, storeDevA)
	if err != nil {
		t.Fatalf("DeviceFolders: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the deleted folder is still authorized: %v", got)
	}
}

func TestDeleteDevice_TakesItsAuthorizationsWithIt(t *testing.T) {
	ctx := context.Background()
	store := authStore(t)
	if err := store.GrantFolderDevice(ctx, "docs", storeDevA); err != nil {
		t.Fatalf("GrantFolderDevice: %v", err)
	}

	if err := store.DeleteDevice(ctx, storeDevA); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	access, err := store.FolderAccess(ctx, "docs")
	if err != nil {
		t.Fatalf("FolderAccess: %v", err)
	}
	if !access.Empty() {
		t.Fatalf("the removed device still allows %v", access.Devices())
	}
}

// writeVersion7DB creates a database at the version before folder-level
// authorization existed, holding two folders and two devices: a deployment
// where every paired device could reach every folder.
func writeVersion7DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	for v := 1; v <= 7; v++ {
		script, err := sqlitestore.MigrationSQL(v)
		if err != nil {
			t.Fatalf("read migration %d: %v", v, err)
		}
		if _, err := db.ExecContext(ctx, script); err != nil {
			t.Fatalf("apply migration %d: %v", v, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO schema_version (version) VALUES (7)`,
		`INSERT INTO folders (id, path, label, mode, delete_policy, include_hidden, symlink_policy,
			keep_empty_dirs, paused, last_error, created_at)
		 VALUES ('docs', '/srv/docs', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, '', 1),
			('pics', '/srv/pics', '', 'sendreceive', 'propagate', 0, 'store', 1, 0, '', 1)`,
		`INSERT INTO devices (id, name, address, verified, added_at)
		 VALUES ('` + string(storeDevA) + `', 'a', '10.0.0.2:22000', 1, 1),
			('` + string(storeDevB) + `', 'b', '10.0.0.3:22000', 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare version 7 database: %v", err)
		}
	}
	return path
}

// TestMigration008_GrantsEveryExistingFolderToEveryKnownDevice is the upgrade
// promise: before this schema a paired device could reach every folder, so the
// backfill is what the deployment already had. An upgrade must not stop a
// synchronization that was running.
func TestMigration008_GrantsEveryExistingFolderToEveryKnownDevice(t *testing.T) {
	ctx := context.Background()
	path := writeVersion7DB(t)

	store, err := sqlitestore.Open(ctx, path, sqlitestore.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	for _, id := range []folder.ID{"docs", "pics"} {
		access, err := store.FolderAccess(ctx, id)
		if err != nil {
			t.Fatalf("FolderAccess %s: %v", id, err)
		}
		for _, dev := range []device.ID{storeDevA, storeDevB} {
			if !access.Allows(dev) {
				t.Errorf("%s does not allow %s after the upgrade", id, dev.Short())
			}
		}
	}
	if got := schemaVersionOf(t, path); got != sqlitestore.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", got, sqlitestore.SchemaVersion)
	}

	// A folder added after the upgrade starts authorized for nobody: the
	// backfill is a one-off, not a rule.
	if err := store.SaveFolder(ctx, testFolder("new", "/srv/new")); err != nil {
		t.Fatalf("SaveFolder: %v", err)
	}
	access, err := store.FolderAccess(ctx, "new")
	if err != nil {
		t.Fatalf("FolderAccess of the new folder: %v", err)
	}
	if !access.Empty() {
		t.Fatalf("a folder added after the upgrade allows %v", access.Devices())
	}
}
