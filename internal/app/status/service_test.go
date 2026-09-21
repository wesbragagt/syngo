package status_test

import (
	"context"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// at is a fixed timestamp used by the status tests.
var at = time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC)

// newService returns a status service over a populated fake store.
func newService(t *testing.T) (*status.Service, *fakes.Store) {
	t.Helper()
	ctx := context.Background()
	store := fakes.NewStore()

	f := folder.Folder{
		ID: "docs", Path: "/home/u/docs", Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkSkip,
		KeepEmptyDirs: true, CreatedAt: at, LastScanAt: at.Add(time.Minute),
	}
	if err := store.SaveFolder(ctx, f); err != nil {
		t.Fatalf("SaveFolder error: %v", err)
	}
	entry := manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Size: 10, ModTime: at}
	if err := store.UpsertEntries(ctx, "docs", []manifest.Entry{entry}); err != nil {
		t.Fatalf("UpsertEntries error: %v", err)
	}
	if err := store.SaveDevice(ctx, device.Device{ID: "PEERID", Name: "nas", Address: "10.0.0.2:22000", AddedAt: at}); err != nil {
		t.Fatalf("SaveDevice error: %v", err)
	}
	big := manifest.Entry{Path: "big.iso", Kind: manifest.KindFile, Size: 2 * transfer.MinChunkSize, ModTime: at}
	tr := transfer.NewInbound("t1", "docs", big, "PEERID", transfer.MinChunkSize, "/tmp/t1.part", at)
	tr.State = transfer.StateActive
	if err := tr.MarkVerified(0, manifest.Hash{1}, at); err != nil {
		t.Fatalf("MarkVerified error: %v", err)
	}
	if err := store.SaveTransfer(ctx, tr); err != nil {
		t.Fatalf("SaveTransfer error: %v", err)
	}

	svc := status.New(store, fakes.NewIdentity("DEVICEID", "laptop"), fakes.NewClock(at), "/data")
	return svc, store
}

func TestReport_WithoutDaemon(t *testing.T) {
	svc, _ := newService(t)

	report, err := svc.Report(context.Background(), nil)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}

	if report.SchemaVersion != status.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", report.SchemaVersion, status.SchemaVersion)
	}
	if report.Device.ID != "DEVICEID" || report.Device.Name != "laptop" || report.Device.DataDir != "/data" {
		t.Fatalf("device = %+v", report.Device)
	}
	if report.Daemon.Running || report.Daemon.StartedAt != nil {
		t.Fatalf("daemon = %+v, want not running", report.Daemon)
	}
	if len(report.Folders) != 1 {
		t.Fatalf("folders = %+v, want one", report.Folders)
	}
	f := report.Folders[0]
	if f.State != status.StateIdle || f.Files != 1 || f.Bytes != 10 || f.Error != nil {
		t.Fatalf("folder = %+v", f)
	}
	if f.LastScanAt == nil || *f.LastScanAt != "2026-09-20T10:01:00Z" {
		t.Fatalf("last_scan_at = %v, want an RFC3339 UTC string", f.LastScanAt)
	}
	if len(report.Peers) != 1 || report.Peers[0].Connected || report.Peers[0].LastSeenAt != nil {
		t.Fatalf("peers = %+v", report.Peers)
	}
	if report.Transfers.Active != 1 || len(report.Transfers.Items) != 1 {
		t.Fatalf("transfers = %+v", report.Transfers)
	}
	item := report.Transfers.Items[0]
	if item.ChunksTotal != 2 || item.ChunksDone != 1 || item.BytesDone != transfer.MinChunkSize ||
		item.Direction != "in" || item.State != string(transfer.StateActive) {
		t.Fatalf("transfer item = %+v", item)
	}
	// The items are the objects `transfer list` prints, which docs/CLI.md
	// promises, so they carry the same timestamps.
	if item.CreatedAt == nil || *item.CreatedAt != "2026-09-20T10:00:00Z" {
		t.Fatalf("created_at = %v, want the time the transfer started", item.CreatedAt)
	}
	if item.UpdatedAt == nil {
		t.Fatal("updated_at is missing from the status transfer item")
	}
	if report.Conflicts.Unresolved != 0 {
		t.Fatalf("conflicts = %+v, want none", report.Conflicts)
	}
}

func TestReport_WithDaemonAndLivePeers(t *testing.T) {
	svc, _ := newService(t)
	live := &status.LiveInfo{
		PID: 4242, Listen: "127.0.0.1:22000", StartedAt: at,
		Peers:        map[string]status.PeerLive{"PEERID": {Connected: true, Latency: 420 * time.Microsecond}},
		FolderStates: map[string]string{"docs": status.StateScanning},
	}

	report, err := svc.Report(context.Background(), live)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}

	if !report.Daemon.Running || report.Daemon.PID != 4242 || report.Daemon.Listen != "127.0.0.1:22000" {
		t.Fatalf("daemon = %+v", report.Daemon)
	}
	if report.Daemon.StartedAt == nil || *report.Daemon.StartedAt != "2026-09-20T10:00:00Z" {
		t.Fatalf("started_at = %v", report.Daemon.StartedAt)
	}
	if report.Folders[0].State != status.StateScanning {
		t.Fatalf("folder state = %q, want scanning", report.Folders[0].State)
	}
	peer := report.Peers[0]
	if !peer.Connected || peer.LatencyMS != 0.42 {
		t.Fatalf("peer = %+v", peer)
	}
}

func TestReport_PausedAndErrorStates(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)

	if err := store.SetFolderScan(ctx, "docs", at, "scan failed"); err != nil {
		t.Fatalf("SetFolderScan error: %v", err)
	}
	report, err := svc.Report(ctx, nil)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	if report.Folders[0].State != status.StateError || report.Folders[0].Error == nil {
		t.Fatalf("folder = %+v, want error state", report.Folders[0])
	}

	if err := store.SetFolderPaused(ctx, "docs", true); err != nil {
		t.Fatalf("SetFolderPaused error: %v", err)
	}
	report, err = svc.Report(ctx, nil)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	if report.Folders[0].State != status.StatePaused {
		t.Fatalf("folder state = %q, want paused", report.Folders[0].State)
	}
}

func TestReport_CountsConflicts(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)
	if err := store.SaveConflict(ctx, ports.ConflictRecord{ID: "c1", FolderID: "docs", Path: "a.txt", DetectedAt: at}); err != nil {
		t.Fatalf("SaveConflict error: %v", err)
	}

	report, err := svc.Report(ctx, nil)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	if report.Conflicts.Unresolved != 1 {
		t.Fatalf("conflicts = %+v, want one unresolved", report.Conflicts)
	}
}
