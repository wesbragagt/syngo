package network_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/network"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
)

// peer is a configured device reached at addr.
func peer(id, addr string) device.Device {
	return device.Device{ID: device.ID(id), Name: id, Address: addr}
}

func TestPeerPathsReadsDirectAndRelayFromTheClient(t *testing.T) {
	nas := fakes.TailnetNode("nas", "100.64.0.3")
	phone := fakes.TailnetNode("phone", "100.64.0.4")
	tailnet := fakes.NewTailnet(nas, phone)
	tailnet.SetPaths(
		fakes.TailnetDirectPath(nas, "192.168.1.20:41641"),
		fakes.TailnetRelayPath(phone, "fra"),
	)
	service := network.New(tailnet)

	devices := []device.Device{
		peer("NASDEVICE", "100.64.0.3:22000"),
		peer("PHONEDEVICE", "phone.tail1234.ts.net:22000"),
	}
	paths, err := service.PeerPaths(context.Background(), devices)
	if err != nil {
		t.Fatalf("peer paths: %v", err)
	}

	direct := paths["NASDEVICE"]
	if direct.Kind != ports.TailnetPathDirect || direct.Endpoint != "192.168.1.20:41641" {
		t.Errorf("path to the nas is %+v, want a direct endpoint", direct)
	}
	relay := paths["PHONEDEVICE"]
	if relay.Kind != ports.TailnetPathRelay || relay.Relay != "fra" {
		t.Errorf("path to the phone is %+v, want the DERP region", relay)
	}
}

func TestPeerPathsReportsUnknownWithAReason(t *testing.T) {
	nas := fakes.TailnetNode("nas", "100.64.0.3")
	tailnet := fakes.NewTailnet(nas)
	tailnet.SetPaths(ports.TailnetPath{
		Node: nas, Kind: ports.TailnetPathUnknown,
		Reason: "the local Tailscale client reports neither a direct endpoint nor a relay for this node",
	})
	service := network.New(tailnet)

	devices := []device.Device{
		peer("NASDEVICE", "100.64.0.3:22000"),
		peer("LANDEVICE", "192.168.1.50:22000"),
	}
	paths, err := service.PeerPaths(context.Background(), devices)
	if err != nil {
		t.Fatalf("peer paths: %v", err)
	}

	silent := paths["NASDEVICE"]
	if silent.Kind != ports.TailnetPathUnknown || silent.Reason == "" {
		t.Errorf("path to a node the client says nothing about is %+v, want unknown with a reason", silent)
	}
	offTailnet := paths["LANDEVICE"]
	if offTailnet.Kind != ports.TailnetPathUnknown {
		t.Errorf("path to a peer that is not a tailnet node is %+v, want unknown", offTailnet)
	}
	if !strings.Contains(offTailnet.Reason, "192.168.1.50") {
		t.Errorf("reason %q does not name the address that was looked up", offTailnet.Reason)
	}
}

func TestPeerPathsWithoutATailscaleClient(t *testing.T) {
	service := network.New(fakes.NewTailnetUnavailable())
	devices := []device.Device{peer("NASDEVICE", "100.64.0.3:22000")}

	paths, err := service.PeerPaths(context.Background(), devices)
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want it to report that the tailnet cannot be asked", err)
	}
	got := paths["NASDEVICE"]
	if got.Kind != ports.TailnetPathUnknown || got.Reason == "" {
		t.Errorf("path without a client is %+v, want unknown with the reason", got)
	}
}

func TestPeerPathsWithNoTailnetWiredAtAll(t *testing.T) {
	service := network.New(nil)
	paths, err := service.PeerPaths(context.Background(), []device.Device{peer("NASDEVICE", "nas:22000")})
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want ErrTailnetUnavailable", err)
	}
	if paths["NASDEVICE"].Kind != ports.TailnetPathUnknown {
		t.Errorf("path = %+v, want unknown", paths["NASDEVICE"])
	}
}

func TestPeerPathsMatchesIPv6Addresses(t *testing.T) {
	node := ports.TailnetNode{
		ID: "nnas", HostName: "nas", DNSName: "nas.tail1234.ts.net",
		IPs: []string{"100.64.0.3", "fd7a:115c:a1e0::3"}, Online: true,
	}
	tailnet := fakes.NewTailnet(node)
	tailnet.SetPaths(fakes.TailnetDirectPath(node, "192.168.1.20:41641"))

	paths, err := network.New(tailnet).PeerPaths(context.Background(),
		[]device.Device{peer("NASDEVICE", "[fd7a:115c:a1e0::3]:22000")})
	if err != nil {
		t.Fatalf("peer paths: %v", err)
	}
	if got := paths["NASDEVICE"]; got.Kind != ports.TailnetPathDirect {
		t.Errorf("path over an IPv6 address is %+v, want the node's direct path", got)
	}
}
