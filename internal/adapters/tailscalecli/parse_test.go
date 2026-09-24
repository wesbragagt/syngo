package tailscalecli_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syngo/internal/adapters/tailscalecli"
	"github.com/wesbragagt/syngo/internal/app/ports"
)

// fixture reads one of the recorded `tailscale status --json` documents.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestParse_CurrentFixture(t *testing.T) {
	status, peers, err := tailscalecli.Parse(fixture(t, "status_current.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !status.Running || status.BackendState != "Running" {
		t.Fatalf("status = %+v, want a running backend", status)
	}
	if status.Version != "1.90.9" {
		t.Fatalf("version = %q, want 1.90.9", status.Version)
	}
	if status.MagicDNSSuffix != "tail1234.ts.net" {
		t.Fatalf("suffix = %q, want the one from CurrentTailnet", status.MagicDNSSuffix)
	}
	if status.Self.HostName != "laptop" || status.Self.DNSName != "laptop.tail1234.ts.net" {
		t.Fatalf("self = %+v, want laptop without a trailing dot", status.Self)
	}
	if got := status.Self.IPs; len(got) != 2 || got[0] != "100.64.0.2" {
		t.Fatalf("self ips = %v, want the IPv4 address first", got)
	}

	// Sorted by host name, with self left out.
	var names []string
	for _, p := range peers {
		names = append(names, p.HostName)
	}
	if len(names) != 3 || names[0] != "desktop" || names[1] != "nas" || names[2] != "phone" {
		t.Fatalf("peers = %v, want desktop, nas, phone in that order", names)
	}
	for _, p := range peers {
		if p.HostName == "laptop" {
			t.Fatalf("peers include self: %+v", p)
		}
	}

	nas := peers[1]
	if !nas.Online || nas.OS != "linux" || nas.ID != "nNAS123CNTRL" {
		t.Fatalf("nas = %+v, want an online linux node", nas)
	}
	if got := nas.IPs; len(got) != 2 || got[0] != "100.64.0.3" {
		t.Fatalf("nas ips = %v, want the IPv4 address first", got)
	}
	if phone := peers[2]; phone.Online {
		t.Fatalf("phone = %+v, want Online honoured as false", phone)
	}
}

func TestParse_OldFixtureFallsBackToActiveAndTopLevelSuffix(t *testing.T) {
	status, peers, err := tailscalecli.Parse(fixture(t, "status_old.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if status.MagicDNSSuffix != "tail1234.ts.net" {
		t.Fatalf("suffix = %q, want the top-level one", status.MagicDNSSuffix)
	}
	if !status.Running {
		t.Fatalf("status = %+v, want running", status)
	}
	if len(peers) != 2 {
		t.Fatalf("peers = %+v, want two", peers)
	}
	// No Online field anywhere in this document: Active decides.
	if !peers[0].Online || peers[0].HostName != "nas" {
		t.Fatalf("nas = %+v, want online from Active", peers[0])
	}
	if peers[1].Online || peers[1].HostName != "phone" {
		t.Fatalf("phone = %+v, want offline from Active", peers[1])
	}
}

func TestParse_StoppedFixtureIsNotRunning(t *testing.T) {
	status, peers, err := tailscalecli.Parse(fixture(t, "status_stopped.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if status.Running || status.BackendState != "Stopped" {
		t.Fatalf("status = %+v, want a stopped backend", status)
	}
	if len(status.Self.IPs) != 0 {
		t.Fatalf("self ips = %v, want none", status.Self.IPs)
	}
	if len(peers) != 0 {
		t.Fatalf("peers = %+v, want none", peers)
	}
}

func TestParse_UnknownFieldsAreIgnored(t *testing.T) {
	raw := []byte(`{
	  "Version": "9.9.9", "BackendState": "Running", "SomethingNew": {"a": [1, 2]},
	  "MagicDNSSuffix": "tail1234.ts.net",
	  "Self": {"ID": "nSELF", "HostName": "laptop", "DNSName": "laptop.tail1234.ts.net.",
	           "TailscaleIPs": ["100.64.0.2"], "Online": true, "OS": "linux", "Future": 1},
	  "Peer": {"nodekey:aa": {"ID": "nNAS", "HostName": "nas", "TailscaleIPs": ["100.64.0.3"],
	           "Online": true, "OS": "linux", "AlsoNew": "x"}}
	}`)
	status, peers, err := tailscalecli.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if status.Self.HostName != "laptop" || len(peers) != 1 || peers[0].HostName != "nas" {
		t.Fatalf("status = %+v peers = %+v, want the known fields read", status, peers)
	}
}

func TestParse_GarbageIsAnError(t *testing.T) {
	for _, raw := range []string{"", "not json", "[]", `{"BackendState": "Running"}`} {
		_, _, err := tailscalecli.Parse([]byte(raw))
		if !errors.Is(err, ports.ErrTailnetUnavailable) {
			t.Fatalf("Parse(%q) error = %v, want ErrTailnetUnavailable", raw, err)
		}
	}
}
