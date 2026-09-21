package sqlitestore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

func TestDevices_CRUD(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)

	d := device.Device{ID: "PEERID", Name: "nas", Address: "100.64.0.2:22000", Verified: true, AddedAt: testTime}
	if err := store.SaveDevice(ctx, d); err != nil {
		t.Fatalf("SaveDevice error: %v", err)
	}

	got, err := store.GetDevice(ctx, "PEERID")
	if err != nil {
		t.Fatalf("GetDevice error: %v", err)
	}
	if got.Name != "nas" || got.Address != d.Address || !got.Verified ||
		!got.AddedAt.Equal(testTime) || !got.LastSeenAt.IsZero() {
		t.Fatalf("device = %+v, want %+v", got, d)
	}

	seen := testTime.Add(time.Minute)
	if err := store.TouchDevice(ctx, "PEERID", seen); err != nil {
		t.Fatalf("TouchDevice error: %v", err)
	}
	list, err := store.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices error: %v", err)
	}
	if len(list) != 1 || !list[0].LastSeenAt.Equal(seen) {
		t.Fatalf("list = %+v, want one device seen at %v", list, seen)
	}

	if err := store.DeleteDevice(ctx, "PEERID"); err != nil {
		t.Fatalf("DeleteDevice error: %v", err)
	}
	if _, err := store.GetDevice(ctx, "PEERID"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("GetDevice after delete = %v, want ErrNotFound", err)
	}
	if err := store.TouchDevice(ctx, "PEERID", seen); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("TouchDevice on unknown device = %v, want ErrNotFound", err)
	}
}

func TestConflicts_SaveAndList(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)

	rec := ports.ConflictRecord{
		ID: "c1", FolderID: "docs", Path: "a.txt", ConflictPath: "a.sync-conflict.txt",
		LocalHash: hashOf(1), RemoteHash: hashOf(2), PeerID: "PEERID", DetectedAt: testTime,
	}
	if err := store.SaveConflict(ctx, rec); err != nil {
		t.Fatalf("SaveConflict error: %v", err)
	}

	list, err := store.ListConflicts(ctx, "docs", false)
	if err != nil {
		t.Fatalf("ListConflicts error: %v", err)
	}
	if len(list) != 1 || list[0] != rec {
		t.Fatalf("list = %+v, want %+v", list, rec)
	}

	rec.Resolved = true
	if err := store.SaveConflict(ctx, rec); err != nil {
		t.Fatalf("SaveConflict error: %v", err)
	}
	unresolved, err := store.ListConflicts(ctx, "", false)
	if err != nil {
		t.Fatalf("ListConflicts error: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want none", unresolved)
	}
	all, err := store.ListConflicts(ctx, "", true)
	if err != nil {
		t.Fatalf("ListConflicts error: %v", err)
	}
	if len(all) != 1 || !all[0].Resolved {
		t.Fatalf("all = %+v, want one resolved conflict", all)
	}
}
