package peers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/peers"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// Device IDs used by the tests; only the syntax matters here.
const (
	peerID  = device.ID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	selfID  = device.ID("CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
	otherID = device.ID("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
)

// now is the time the fake clock reports.
var now = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

// fixture is a peer service over fakes.
type fixture struct {
	service *peers.Service
	store   *fakes.Store
	dialer  *fakes.Dialer
}

// newFixture returns a service whose dialer answers for "nas:22000".
func newFixture(t *testing.T, probe ports.ProbeResult) fixture {
	t.Helper()
	store := fakes.NewStore()
	dialer := fakes.NewDialer()
	dialer.Set("nas:22000", fakes.PeerScript{Probe: probe})
	return fixture{
		service: peers.New(store, dialer, fakes.NewClock(now), fakes.NewIdentity(selfID, "self"), 22000),
		store:   store,
		dialer:  dialer,
	}
}

// accept returns yes to every confirmation request.
func accept(device.ID, string) (bool, error) { return true, nil }

func TestAdd_TOFUStoresProbeID(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: false, RejectReason: "unknown_device"})

	result, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", Confirm: accept})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if result.Device.ID != peerID || result.Device.Address != "nas:22000" {
		t.Fatalf("device = %+v, want the probed id at the default port", result.Device)
	}
	if !result.Device.Verified || !result.Device.AddedAt.Equal(now) {
		t.Fatalf("device = %+v, want it verified and stamped", result.Device)
	}
	if result.Accepted {
		t.Fatal("Accepted = true, want the peer not to know us yet")
	}

	stored, err := f.store.GetDevice(context.Background(), peerID)
	if err != nil || stored.Name != "nas" {
		t.Fatalf("stored device = %+v, err %v", stored, err)
	}
}

func TestAdd_ExpectIDSkipsConfirmationAndPinsIdentity(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})

	result, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas:22000", ExpectID: peerID})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if !result.Accepted || !result.Device.Verified {
		t.Fatalf("result = %+v, want an accepted, verified peer", result)
	}
}

func TestAdd_ExpectIDMismatch(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})

	_, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", ExpectID: otherID})
	if !errors.Is(err, ports.ErrDeviceMismatch) {
		t.Fatalf("Add error = %v, want ErrDeviceMismatch", err)
	}
	if _, err := f.store.GetDevice(context.Background(), peerID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("the peer was stored despite the mismatch")
	}
}

func TestAdd_VerifyRequiresConfirm(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})

	asked := make(chan device.ID, 1)
	_, err := f.service.Add(context.Background(), peers.AddOptions{
		Address:  "nas",
		ExpectID: peerID,
		Verify:   true,
		Confirm: func(id device.ID, _ string) (bool, error) {
			asked <- id
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	select {
	case id := <-asked:
		if id != peerID {
			t.Fatalf("confirmed %s, want %s", id, peerID)
		}
	default:
		t.Fatal("--verify did not ask for confirmation")
	}

	// Without a confirmation callback there is nobody to trust the peer.
	f2 := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas"})
	if _, err := f2.service.Add(context.Background(), peers.AddOptions{Address: "nas"}); !errors.Is(err, peers.ErrNotConfirmed) {
		t.Fatalf("Add without Confirm = %v, want ErrNotConfirmed", err)
	}
}

func TestAdd_VerifyDeclined(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})

	decline := func(device.ID, string) (bool, error) { return false, nil }
	if _, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", Confirm: decline}); !errors.Is(err, peers.ErrNotConfirmed) {
		t.Fatalf("Add declined = %v, want ErrNotConfirmed", err)
	}
	if _, err := f.store.GetDevice(context.Background(), peerID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("a declined peer was stored")
	}
}

func TestAdd_UnreachableAddress(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID})

	_, err := f.service.Add(context.Background(), peers.AddOptions{Address: "ghost", Confirm: accept})
	if !errors.Is(err, ports.ErrPeerUnreachable) {
		t.Fatalf("Add error = %v, want ErrPeerUnreachable", err)
	}
}

func TestAdd_KnownPeerAlreadyExists(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})
	ctx := context.Background()
	if _, err := f.service.Add(ctx, peers.AddOptions{Address: "nas", Confirm: accept}); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	if _, err := f.service.Add(ctx, peers.AddOptions{Address: "nas", Confirm: accept}); !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("second Add = %v, want ErrAlreadyExists", err)
	}
}

func TestListAndRemove(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})
	ctx := context.Background()
	if _, err := f.service.Add(ctx, peers.AddOptions{Address: "nas", Confirm: accept}); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	list, err := f.service.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, err %v", list, err)
	}
	// The dashed display form is what `peer list` prints, so it must work.
	if err := f.service.Remove(ctx, device.ID(peerID.Display())); err != nil {
		t.Fatalf("Remove error: %v", err)
	}
	if list, err = f.service.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List after Remove = %v, err %v", list, err)
	}
	if err := f.service.Remove(ctx, peerID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Remove unknown = %v, want ErrNotFound", err)
	}
}

func TestTest_ReportsRejectReason(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", RejectReason: "unknown_device"})

	result, err := f.service.Test(context.Background(), "nas")
	if err != nil {
		t.Fatalf("Test error: %v", err)
	}
	if !result.TLSOK || result.Authenticated {
		t.Fatalf("result = %+v, want a TLS connection the peer refused", result)
	}
	if result.Reason != "rejected:unknown_device" || result.DeviceID != peerID {
		t.Fatalf("result = %+v, want the reject reason and the probed id", result)
	}
}

// TestTest_ProtocolVersionMismatchIsARejection covers the reason a version
// mismatch gets: the handshake did not break, the connection was refused for a
// protocol reason, whichever of the two sides noticed it.
func TestTest_ProtocolVersionMismatchIsARejection(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID})
	f.dialer.Set("nas:22000", fakes.PeerScript{Err: &ports.ConnectError{
		Stage: ports.StageHandshake,
		Err: &ports.RejectedError{
			Reason: "protocol_version",
			Detail: "peer speaks protocol version 1, want 2",
		},
	}})

	result, err := f.service.Test(context.Background(), "nas")
	if err != nil {
		t.Fatalf("Test error: %v", err)
	}
	if result.Reason != "rejected:protocol_version" {
		t.Fatalf("reason = %q, want rejected:protocol_version", result.Reason)
	}
	if result.Err != "peer speaks protocol version 1, want 2" {
		t.Fatalf("error = %q, want it without the stage prefix the reason already carries", result.Err)
	}
}

func TestTest_UnreachableReportsDialStage(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID})

	result, err := f.service.Test(context.Background(), "ghost:1")
	if err != nil {
		t.Fatalf("Test error: %v", err)
	}
	if result.TLSOK || result.Reason != ports.StageDial || result.Err == "" {
		t.Fatalf("result = %+v, want a dial failure", result)
	}
}

func TestTest_KnownPeerByIDTouchesLastSeen(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true, Latency: 3 * time.Millisecond})
	ctx := context.Background()
	if _, err := f.service.Add(ctx, peers.AddOptions{Address: "nas", Confirm: accept}); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	result, err := f.service.Test(ctx, peerID.Display())
	if err != nil {
		t.Fatalf("Test error: %v", err)
	}
	if !result.Authenticated || result.Latency != 3*time.Millisecond || result.Address != "nas:22000" {
		t.Fatalf("result = %+v, want an authenticated peer with its latency", result)
	}

	stored, err := f.store.GetDevice(ctx, peerID)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if !stored.LastSeenAt.Equal(now) {
		t.Fatalf("LastSeenAt = %v, want %v", stored.LastSeenAt, now)
	}
}

func TestTest_UnknownDeviceIDIsNotFound(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID})

	if _, err := f.service.Test(context.Background(), string(otherID)); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Test error = %v, want ErrNotFound", err)
	}
}

func TestAdd_CompletesTheAddressWithTheDefaultPort(t *testing.T) {
	cases := map[string]string{
		"nas":         "nas:22000",
		"nas:9999":    "nas:9999",
		"::1":         "[::1]:22000",
		"[::1]":       "[::1]:22000",
		"[::1]:22000": "[::1]:22000",
		"2001:db8::1": "[2001:db8::1]:22000",
	}
	for given, want := range cases {
		f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})
		f.dialer.Set(want, fakes.PeerScript{Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true}})

		result, err := f.service.Add(context.Background(), peers.AddOptions{Address: given, Confirm: accept})
		if err != nil {
			t.Fatalf("Add(%q) error: %v", given, err)
		}
		if result.Device.Address != want {
			t.Fatalf("Add(%q) stored %q, want %q", given, result.Device.Address, want)
		}
	}
}

func TestAdd_RefusesThisDeviceItself(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: selfID, Name: "self", Accepted: true})

	_, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", Confirm: accept})
	if !errors.Is(err, peers.ErrSelf) {
		t.Fatalf("Add error = %v, want ErrSelf", err)
	}
	list, err := f.store.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("stored %d peers, want none", len(list))
	}
}

func TestAdd_InvalidAddressIsReported(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})

	if _, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas:", Confirm: accept}); !errors.Is(err, peers.ErrInvalidAddress) {
		t.Fatalf("Add error = %v, want ErrInvalidAddress", err)
	}
	if _, err := f.service.Test(context.Background(), "nas:"); !errors.Is(err, peers.ErrInvalidAddress) {
		t.Fatalf("Test error = %v, want ErrInvalidAddress", err)
	}
}

// TestAdd_AddressThatIsNotAnIPv6LiteralIsReported covers what looks like a
// bare IPv6 address but is not one. It is a typo, not a peer that is down, so
// it is refused before anything is dialled.
func TestAdd_AddressThatIsNotAnIPv6LiteralIsReported(t *testing.T) {
	for _, target := range []string{":::", "[:::]", "::1::2", "[gg::1]:22000", "1:2:3:4:5:6:7"} {
		f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})

		_, err := f.service.Add(context.Background(), peers.AddOptions{Address: target, Confirm: accept})
		if !errors.Is(err, peers.ErrInvalidAddress) {
			t.Fatalf("Add(%q) error = %v, want ErrInvalidAddress", target, err)
		}
		if probed := f.dialer.Probed(); len(probed) != 0 {
			t.Fatalf("Add(%q) probed %v, want nothing dialled", target, probed)
		}
	}
}
