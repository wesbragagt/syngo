package peers_test

import (
	"context"
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/peers"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// tailnetFixture is a peer service whose tailnet knows "nas" at 100.64.0.3.
func tailnetFixture(t *testing.T, tailnet ports.TailnetDiscovery, dialAddr string) fixture {
	t.Helper()
	store := fakes.NewStore()
	dialer := fakes.NewDialer()
	dialer.Set(dialAddr, fakes.PeerScript{
		Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
	})
	service := peers.New(store, dialer, fakes.NewClock(now), fakes.NewIdentity(selfID, "self"), 22000)
	service.SetTailnet(tailnet)
	return fixture{service: service, store: store, dialer: dialer}
}

func TestAdd_ResolvesHostnameThroughTailnetAndStoresTheIP(t *testing.T) {
	f := tailnetFixture(t, fakes.NewTailnet(fakes.TailnetNode("nas", "100.64.0.3")), "100.64.0.3:22000")

	result, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", Confirm: accept})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if result.Device.Address != "100.64.0.3:22000" {
		t.Fatalf("address = %q, want the tailnet address", result.Device.Address)
	}
	if result.ResolvedFrom != "nas" {
		t.Fatalf("resolved from = %q, want nas", result.ResolvedFrom)
	}

	stored, err := f.store.GetDevice(context.Background(), peerID)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if stored.Address != "100.64.0.3:22000" {
		t.Fatalf("stored address = %q, want the resolved one", stored.Address)
	}
}

func TestAdd_ResolvesAMagicDNSNameAndKeepsAnExplicitPort(t *testing.T) {
	f := tailnetFixture(t, fakes.NewTailnet(fakes.TailnetNode("nas", "100.64.0.3")), "100.64.0.3:23000")

	result, err := f.service.Add(context.Background(),
		peers.AddOptions{Address: "nas.tail1234.ts.net:23000", Confirm: accept})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if result.Device.Address != "100.64.0.3:23000" || result.ResolvedFrom != "nas.tail1234.ts.net" {
		t.Fatalf("result = %+v, want the port kept and the name recorded", result)
	}
}

func TestAdd_FallsBackToDNSWhenTailnetUnavailable(t *testing.T) {
	f := tailnetFixture(t, fakes.NewTailnetUnavailable(), "nas:22000")

	result, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", Confirm: accept})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if result.Device.Address != "nas:22000" || result.ResolvedFrom != "" {
		t.Fatalf("result = %+v, want the host left as typed", result)
	}
}

func TestAdd_FallsBackWhenNameUnknownToTailnet(t *testing.T) {
	f := tailnetFixture(t, fakes.NewTailnet(fakes.TailnetNode("other", "100.64.0.9")), "nas:22000")

	result, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", Confirm: accept})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if result.Device.Address != "nas:22000" || result.ResolvedFrom != "" {
		t.Fatalf("result = %+v, want the host left as typed", result)
	}
}

func TestAdd_IPLiteralNeverAsksTailnet(t *testing.T) {
	tailnet := fakes.NewTailnet(fakes.TailnetNode("nas", "100.64.0.3"))
	f := tailnetFixture(t, tailnet, "100.64.0.3:22000")

	if _, err := f.service.Add(context.Background(),
		peers.AddOptions{Address: "100.64.0.3", Confirm: accept}); err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if got := tailnet.Resolves(); got != 0 {
		t.Fatalf("resolve calls = %d, want none for an address literal", got)
	}
}

func TestAdd_IPv6LiteralNeverAsksTailnet(t *testing.T) {
	tailnet := fakes.NewTailnet()
	f := tailnetFixture(t, tailnet, "[fd7a:115c:a1e0::3]:22000")

	if _, err := f.service.Add(context.Background(),
		peers.AddOptions{Address: "fd7a:115c:a1e0::3", Confirm: accept}); err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if got := tailnet.Resolves(); got != 0 {
		t.Fatalf("resolve calls = %d, want none for an address literal", got)
	}
}

func TestAdd_NoTailnetConfiguredBehavesAsBefore(t *testing.T) {
	f := newFixture(t, ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true})

	result, err := f.service.Add(context.Background(), peers.AddOptions{Address: "nas", Confirm: accept})
	if err != nil {
		t.Fatalf("Add error: %v", err)
	}
	if result.Device.Address != "nas:22000" || result.ResolvedFrom != "" {
		t.Fatalf("result = %+v, want the host left as typed", result)
	}
}

func TestTest_ResolvesHostnameToo(t *testing.T) {
	f := tailnetFixture(t, fakes.NewTailnet(fakes.TailnetNode("nas", "100.64.0.3")), "100.64.0.3:22000")

	result, err := f.service.Test(context.Background(), "nas")
	if err != nil {
		t.Fatalf("Test error: %v", err)
	}
	if result.Address != "100.64.0.3:22000" || result.ResolvedFrom != "nas" {
		t.Fatalf("result = %+v, want the resolved address", result)
	}
	if !result.Authenticated {
		t.Fatalf("result = %+v, want the probe to have reached the peer", result)
	}
}

func TestTest_KnownDeviceIDIsNotResolved(t *testing.T) {
	tailnet := fakes.NewTailnet(fakes.TailnetNode("nas", "100.64.0.3"))
	f := tailnetFixture(t, tailnet, "nas:22000")
	known := device.Device{ID: peerID, Name: "nas", Address: "nas:22000", Verified: true, AddedAt: now}
	if err := f.store.SaveDevice(context.Background(), known); err != nil {
		t.Fatalf("save device: %v", err)
	}

	result, err := f.service.Test(context.Background(), string(peerID))
	if err != nil {
		t.Fatalf("Test error: %v", err)
	}
	// The stored address is used as it stands: it was resolved when the
	// peer was added.
	if result.Address != "nas:22000" || result.ResolvedFrom != "" {
		t.Fatalf("result = %+v, want the stored address", result)
	}
	if got := tailnet.Resolves(); got != 0 {
		t.Fatalf("resolve calls = %d, want none for a known device id", got)
	}
}
