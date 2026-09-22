package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/daemon"
	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/network"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// staticListenState reports a fixed listener state, the way a rebinder does.
func staticListenState(state network.ListenState) func() network.ListenState {
	return func() network.ListenState { return state }
}

func TestStatusReportsTheListenerItIsBoundTo(t *testing.T) {
	lost := time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC)
	state := network.ListenState{
		Bound: true, Address: "100.64.0.2:22000", Interface: "tailscale0",
		BoundAt: lost.Add(time.Minute), LostAt: lost, Rebinds: 2,
	}
	h := startDaemonTweaked(t, daemon.Config{Listen: "100.64.0.2:0"}, nil,
		func(d *daemon.Deps) { d.ListenState = staticListenState(state) }, nil)

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	listener := report.Daemon.Listener
	switch {
	case report.Daemon.Listen != "100.64.0.2:22000":
		t.Errorf("daemon.listen = %q, want the address the listener is bound to", report.Daemon.Listen)
	case !listener.Bound:
		t.Error("listener is reported as not bound")
	case listener.Interface != "tailscale0":
		t.Errorf("listener interface = %q, want tailscale0", listener.Interface)
	case listener.LostAt == nil || listener.BoundAt == nil:
		t.Errorf("listener times = %+v, want when it was lost and when it came back", listener)
	case listener.Rebinds != 2:
		t.Errorf("listener rebinds = %d, want 2", listener.Rebinds)
	}
}

func TestStatusReportsARebindingListener(t *testing.T) {
	state := network.ListenState{
		Bound: false, Address: "100.64.0.2:22000", Interface: "tailscale0",
		Failures: 3, Error: "bind: cannot assign requested address",
	}
	h := startDaemonTweaked(t, daemon.Config{Listen: "100.64.0.2:22000"}, nil,
		func(d *daemon.Deps) { d.ListenState = staticListenState(state) }, nil)

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	listener := report.Daemon.Listener
	switch {
	case listener.Bound:
		t.Error("listener is reported as bound while it is rebinding")
	case listener.Failures != 3:
		t.Errorf("listener failures = %d, want 3", listener.Failures)
	case listener.Error == nil:
		t.Error("listener error is null, want why the last bind failed")
	}
}

// scriptedPaths is a daemon.PathProbe answering from a fixed map.
type scriptedPaths struct {
	paths map[device.ID]ports.TailnetPath
	err   error
}

// PeerPaths returns the scripted answer.
func (p scriptedPaths) PeerPaths(context.Context, []device.Device) (map[device.ID]ports.TailnetPath, error) {
	return p.paths, p.err
}

func TestStatusReportsTheTailnetPathOfEveryPeer(t *testing.T) {
	const peerID = device.ID("CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
	probe := scriptedPaths{paths: map[device.ID]ports.TailnetPath{
		peerID: {Kind: ports.TailnetPathRelay, Relay: "fra", Active: true},
	}}
	h := startDaemonTweaked(t, daemon.Config{PathInterval: 10 * time.Millisecond}, nil,
		func(d *daemon.Deps) { d.Paths = probe }, []device.Device{testPeer(peerID, "nas:22000")})

	waitFor(t, "the path of the peer", func() bool {
		report, err := h.daemon.Status(h.ctx)
		if err != nil || len(report.Peers) != 1 {
			return false
		}
		return report.Peers[0].Path.Kind == status.PathRelay
	})

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	path := report.Peers[0].Path
	if path.Relay != "fra" || !path.Active || path.ObservedAt == nil {
		t.Errorf("peer path = %+v, want the relay region, active, with the time it was read", path)
	}
}

func TestStatusNeverGuessesAPathTheClientCannotGive(t *testing.T) {
	const peerID = device.ID("DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD")
	probe := scriptedPaths{
		paths: map[device.ID]ports.TailnetPath{peerID: {
			Kind: ports.TailnetPathUnknown, Reason: "the local Tailscale client could not be asked",
		}},
		err: ports.ErrTailnetUnavailable,
	}
	h := startDaemonTweaked(t, daemon.Config{PathInterval: 10 * time.Millisecond}, nil,
		func(d *daemon.Deps) { d.Paths = probe }, []device.Device{testPeer(peerID, "nas:22000")})

	waitFor(t, "the unknown path of the peer", func() bool {
		report, err := h.daemon.Status(h.ctx)
		if err != nil || len(report.Peers) != 1 {
			return false
		}
		return report.Peers[0].Path.Reason != nil
	})

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	path := report.Peers[0].Path
	if path.Kind != status.PathUnknown || path.Endpoint != "" || path.Relay != "" {
		t.Errorf("peer path = %+v, want unknown and nothing invented", path)
	}
}

func TestUnreachablePeerIsRetriedWithABoundedBackoff(t *testing.T) {
	const peerID = device.ID("EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE")
	h := startDaemonWithPeers(t, daemon.Config{}, []device.Device{testPeer(peerID, "ghost:22000")})

	waitFor(t, "the peer to be reported unreachable", func() bool {
		report, err := h.daemon.Status(h.ctx)
		if err != nil || len(report.Peers) != 1 {
			return false
		}
		return report.Peers[0].State == status.PeerUnreachable
	})

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	peer := report.Peers[0]
	if peer.NextRetryAt == nil {
		t.Fatal("an unreachable peer reports no next_retry_at, so nothing says the retry is bounded")
	}
	if peer.Error == nil {
		t.Error("an unreachable peer reports no error")
	}

	// The dialler backs off: a peer that stays unreachable must not be
	// dialled in a tight loop for as long as the daemon runs.
	time.Sleep(200 * time.Millisecond)
	if dials := len(h.dialer.Dialed()); dials > 5 {
		t.Errorf("the peer was dialled %d times in 200ms, which is spinning", dials)
	}
}

func TestConnectedPeerReportsItsState(t *testing.T) {
	const peerID = device.ID("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: 50 * time.Millisecond}, nil)
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
	})
	if err := h.store.SaveDevice(h.ctx, testPeer(peerID, "nas:22000")); err != nil {
		t.Fatalf("save device: %v", err)
	}

	waitFor(t, "the peer to be reported connected", func() bool {
		report, err := h.daemon.Status(h.ctx)
		if err != nil || len(report.Peers) != 1 {
			return false
		}
		return report.Peers[0].State == status.PeerConnected
	})

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if report.Peers[0].NextRetryAt != nil {
		t.Errorf("a connected peer reports next_retry_at %v, want null", *report.Peers[0].NextRetryAt)
	}
}
