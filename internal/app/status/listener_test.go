package status_test

import (
	"context"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/status"
)

// liveListener is a report from a daemon whose listener lost its address once
// and came back.
func liveListener() *status.LiveInfo {
	lost := at.Add(time.Minute)
	bound := at.Add(2 * time.Minute)
	return &status.LiveInfo{
		PID:    42,
		Listen: "100.64.0.2:22000",
		Listener: status.Listener{
			Bound: true, Address: "100.64.0.2:22000", Interface: "tailscale0",
			BoundAt: strPtr(bound), LostAt: strPtr(lost), Rebinds: 1,
		},
		StartedAt: at,
		Peers:     map[string]status.PeerLive{},
	}
}

// strPtr formats a time the way the report does.
func strPtr(t time.Time) *string {
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func TestReportSaysWhatHappenedToTheListener(t *testing.T) {
	svc, _ := newService(t)

	report, err := svc.Report(context.Background(), liveListener())
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	got := report.Daemon.Listener
	switch {
	case !got.Bound:
		t.Error("listener is reported as not bound, want bound")
	case got.Address != "100.64.0.2:22000":
		t.Errorf("listener address = %q, want the bound one", got.Address)
	case got.Interface != "tailscale0":
		t.Errorf("listener interface = %q, want the one it is bound to", got.Interface)
	case got.LostAt == nil:
		t.Error("listener lost_at is null, want the time the address went away")
	case got.BoundAt == nil:
		t.Error("listener bound_at is null, want the time it came back")
	case got.Rebinds != 1:
		t.Errorf("listener rebinds = %d, want 1", got.Rebinds)
	}
}

func TestReportPeerStatesAndPaths(t *testing.T) {
	svc, _ := newService(t)
	live := liveListener()
	retry := at.Add(5 * time.Second)
	live.Peers["PEERID"] = status.PeerLive{
		State:       status.PeerUnreachable,
		Error:       "dial: connection refused",
		NextRetryAt: retry,
		Path: status.PathLive{
			Kind: status.PathRelay, Relay: "fra", Active: true, ObservedAt: at,
		},
	}

	report, err := svc.Report(context.Background(), live)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	peer := report.Peers[0]
	switch {
	case peer.State != status.PeerUnreachable:
		t.Errorf("peer state = %q, want unreachable", peer.State)
	case peer.NextRetryAt == nil || *peer.NextRetryAt != retry.Format(time.RFC3339):
		t.Errorf("peer next_retry_at = %v, want the backoff deadline", peer.NextRetryAt)
	case peer.Path.Kind != status.PathRelay || peer.Path.Relay != "fra":
		t.Errorf("peer path = %+v, want the DERP region the client named", peer.Path)
	case peer.Path.ObservedAt == nil:
		t.Error("peer path observed_at is null, want when the client was asked")
	}
}

func TestReportNeverGuessesAPath(t *testing.T) {
	svc, _ := newService(t)

	// Without a daemon nothing has asked the Tailscale client, and with a
	// daemon that has not dialled the peer yet nothing knows either. Both
	// say unknown, with the reason.
	withoutDaemon, err := svc.Report(context.Background(), nil)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	path := withoutDaemon.Peers[0].Path
	if path.Kind != status.PathUnknown || path.Reason == nil {
		t.Errorf("path without a daemon = %+v, want unknown with a reason", path)
	}
	if got := withoutDaemon.Peers[0].State; got != status.PeerUnknown {
		t.Errorf("peer state without a daemon = %q, want unknown", got)
	}

	withDaemon, err := svc.Report(context.Background(), liveListener())
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	path = withDaemon.Peers[0].Path
	if path.Kind != status.PathUnknown || path.Reason == nil {
		t.Errorf("path before the peer was dialled = %+v, want unknown with a reason", path)
	}
}

func TestReportWithoutARebindingListener(t *testing.T) {
	svc, _ := newService(t)
	live := &status.LiveInfo{PID: 42, Listen: "127.0.0.1:22000", StartedAt: at}

	report, err := svc.Report(context.Background(), live)
	if err != nil {
		t.Fatalf("Report error: %v", err)
	}
	if got := report.Daemon.Listener.Address; got != "" {
		t.Errorf("listener address = %q, want empty when the daemon reported no listener state", got)
	}
	if report.Daemon.Listen != "127.0.0.1:22000" {
		t.Errorf("daemon.listen = %q, want what the daemon reported", report.Daemon.Listen)
	}
}
