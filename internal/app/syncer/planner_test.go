package syncer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/syncer"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/syncplan"
)

// planned returns the plan of the single peer, failing the test when another
// number of peers answered.
func planned(t *testing.T, plans []syncer.PeerPlan) syncer.PeerPlan {
	t.Helper()
	if len(plans) != 1 {
		t.Fatalf("%d peers answered, want 1", len(plans))
	}
	return plans[0]
}

// newPlanner returns a planner for a destination that knows one peer serving
// src at "nas:22000".
func newPlanner(t *testing.T, d *dest, src *source) *syncer.Planner {
	t.Helper()
	dialer := fakes.NewDialer()
	dialer.Set("nas:22000", fakes.PeerScript{
		Probe:   ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
		Handler: src.server,
	})
	err := d.store.SaveDevice(context.Background(), device.Device{
		ID: peerID, Name: "nas", Address: "nas:22000", Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("save device: %v", err)
	}
	return syncer.NewPlanner(d.store, d.fs, dialer)
}

func TestPlanner_ListsWhatWouldBePulled(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	src.addFile(t, "new.bin", payload(chunkSize, 1))

	d, _ := newDest(t, src.server)
	planner := newPlanner(t, d, src)

	plans, err := planner.Plan(context.Background(), d.folder.ID, "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := planned(t, plans)
	if plan.PeerID != peerID || plan.Address != "nas:22000" || plan.Note != "" {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("items = %+v, want one", plan.Items)
	}
	item := plan.Items[0]
	if item.Action != syncplan.ActionPull || item.Path != "new.bin" || item.Reason != syncplan.ReasonMissing {
		t.Fatalf("item = %+v, want a pull of new.bin because it is missing", item)
	}
	if plan.Changes() != 1 {
		t.Fatalf("changes = %d, want 1", plan.Changes())
	}

	// A dry run writes nothing: no file arrived and no journal row exists.
	if _, ok := d.fs.Content(d.folder.Path + "/new.bin"); ok {
		t.Fatal("the dry run installed the file")
	}
	list, err := d.store.ListTransfers(context.Background(), ports.TransferFilter{})
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("the dry run wrote %d journal rows, want none", len(list))
	}
}

func TestPlanner_InSyncFolderHasNoChanges(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize, 2)
	src.addFile(t, "same.bin", data)

	d, conn := newDest(t, src.server)
	d.sync(t, conn)

	planner := newPlanner(t, d, src)
	plans, err := planner.Plan(context.Background(), d.folder.ID, "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := planned(t, plans)
	if plan.Changes() != 0 {
		t.Fatalf("changes = %d after a sync, want none: %+v", plan.Changes(), plan.Items)
	}
}

func TestPlanner_ReportsADeleteTheFolderPolicyKeeps(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	data := payload(chunkSize, 3)
	src.addFile(t, "keep.bin", data)

	d, conn := newDest(t, src.server)
	d.sync(t, conn)

	// The folder never applies deletions, and the peer deleted the file.
	d.folder.DeletePolicy = folder.DeleteNever
	if err := d.store.SaveFolder(context.Background(), d.folder); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	err := src.store.MarkDeleted(context.Background(), src.folder.ID, []string{"keep.bin"}, "SOURCE", time.Now())
	if err != nil {
		t.Fatalf("delete on the source: %v", err)
	}

	planner := newPlanner(t, d, src)
	plans, err := planner.Plan(context.Background(), d.folder.ID, "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := planned(t, plans)
	if len(plan.Items) != 1 {
		t.Fatalf("items = %+v, want the skipped delete", plan.Items)
	}
	item := plan.Items[0]
	if item.Action != syncplan.ActionNone || item.Reason != syncplan.ReasonDeletePolicyNever {
		t.Fatalf("item = %+v, want the delete skipped by policy", item)
	}
	if plan.Changes() != 0 {
		t.Fatalf("changes = %d, want none: a skipped delete changes nothing", plan.Changes())
	}
}

func TestPlanner_PeerThatDoesNotServeTheFolderIsNoted(t *testing.T) {
	src := newSource(t, folder.ModeReceiveOnly)
	src.addFile(t, "hidden.bin", payload(chunkSize, 4))

	d, _ := newDest(t, src.server)
	planner := newPlanner(t, d, src)

	plans, err := planner.Plan(context.Background(), d.folder.ID, "")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := planned(t, plans)
	if plan.Note == "" || len(plan.Items) != 0 {
		t.Fatalf("plan = %+v, want a note and no items", plan)
	}
}

func TestPlanner_UnknownFolderAndPeer(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	d, _ := newDest(t, src.server)
	planner := newPlanner(t, d, src)

	if _, err := planner.Plan(context.Background(), "nope", ""); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown folder = %v, want ErrNotFound", err)
	}
	if _, err := planner.Plan(context.Background(), d.folder.ID, "OTHER"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown peer = %v, want ErrNotFound", err)
	}
}

func TestPlanner_UnreachablePeerFails(t *testing.T) {
	src := newSource(t, folder.ModeSendReceive)
	d, _ := newDest(t, src.server)
	err := d.store.SaveDevice(context.Background(), device.Device{
		ID: "GONE", Name: "gone", Address: "gone:22000", Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("save device: %v", err)
	}
	planner := syncer.NewPlanner(d.store, d.fs, fakes.NewDialer())

	if _, err := planner.Plan(context.Background(), d.folder.ID, "GONE"); !errors.Is(err, ports.ErrPeerUnreachable) {
		t.Fatalf("unreachable peer = %v, want ErrPeerUnreachable", err)
	}
}
