package sqlitestore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
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

func TestConflicts_GetAndResolve(t *testing.T) {
	ctx := context.Background()
	store := storeWithFolder(t)

	rec := ports.ConflictRecord{
		ID: "c1", FolderID: "docs", Path: "a.txt",
		ConflictPath: "a.sync-conflict-20260921-153000-ABCDEFG.txt",
		LocalHash:    hashOf(1), RemoteHash: hashOf(2), PeerID: "PEERID", DetectedAt: testTime,
		Reason: "both-modified",
	}
	if err := store.SaveConflict(ctx, rec); err != nil {
		t.Fatalf("SaveConflict error: %v", err)
	}

	got, err := store.GetConflict(ctx, "c1")
	if err != nil {
		t.Fatalf("GetConflict error: %v", err)
	}
	if got != rec {
		t.Fatalf("conflict = %+v, want %+v", got, rec)
	}

	if err := store.SetConflictResolved(ctx, "c1", true); err != nil {
		t.Fatalf("SetConflictResolved error: %v", err)
	}
	got, err = store.GetConflict(ctx, "c1")
	if err != nil {
		t.Fatalf("GetConflict error: %v", err)
	}
	if !got.Resolved || got.Reason != "both-modified" {
		t.Fatalf("conflict = %+v, want it resolved and its reason kept", got)
	}
	// Resolving is reversible, so a mistake can be taken back.
	if err := store.SetConflictResolved(ctx, "c1", false); err != nil {
		t.Fatalf("SetConflictResolved error: %v", err)
	}
	if got, err = store.GetConflict(ctx, "c1"); err != nil || got.Resolved {
		t.Fatalf("conflict = %+v (%v), want it open again", got, err)
	}

	if _, err := store.GetConflict(ctx, "nope"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("GetConflict of an unknown id = %v, want ErrNotFound", err)
	}
	if err := store.SetConflictResolved(ctx, "nope", true); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("SetConflictResolved of an unknown id = %v, want ErrNotFound", err)
	}
}

// TestCounterFloor_OnlyEverRises covers the floor that keeps a restored
// database from handing out counters its peers already hold.
func TestCounterFloor_OnlyEverRises(t *testing.T) {
	ctx := context.Background()
	store, _ := openStore(t)
	const self = device.ID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	if got, err := store.CounterFloor(ctx, self); err != nil || got != 0 {
		t.Fatalf("floor = %d, err %v, want 0 for a device with no record", got, err)
	}
	if err := store.RaiseCounterFloor(ctx, self, 7); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if err := store.RaiseCounterFloor(ctx, self, 3); err != nil {
		t.Fatalf("raise lower: %v", err)
	}
	if got, err := store.CounterFloor(ctx, self); err != nil || got != 7 {
		t.Fatalf("floor = %d, err %v, want the higher of the two", got, err)
	}
	if err := store.RaiseCounterFloor(ctx, self, 9); err != nil {
		t.Fatalf("raise higher: %v", err)
	}
	if got, _ := store.CounterFloor(ctx, self); got != 9 {
		t.Fatalf("floor = %d, want 9", got)
	}
	// A device without an id has nothing to record and nothing to report.
	if err := store.RaiseCounterFloor(ctx, "", 5); err != nil {
		t.Fatalf("raise without an id: %v", err)
	}
	if got, _ := store.CounterFloor(ctx, ""); got != 0 {
		t.Fatalf("floor without an id = %d, want 0", got)
	}
}
