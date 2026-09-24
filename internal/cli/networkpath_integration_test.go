package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/cli"
	"github.com/wesbragagt/syngo/internal/compose"
	"github.com/wesbragagt/syngo/internal/domain/device"
)

// pathPeerID is the peer the path tests configure.
const pathPeerID = device.ID("ETI3RRZBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")

// peerPathReport is the JSON shape of one row of `network path`.
type peerPathReport struct {
	PeerID   string  `json:"peer_id"`
	Name     string  `json:"name"`
	Address  string  `json:"address"`
	Kind     string  `json:"kind"`
	Endpoint string  `json:"endpoint"`
	Relay    string  `json:"relay"`
	Active   bool    `json:"active"`
	Reason   *string `json:"reason"`
}

// savePathPeer writes a peer row reached at the given address.
func savePathPeer(t *testing.T, e env, address string) {
	t.Helper()
	ctx := context.Background()
	app, err := compose.Build(ctx, compose.Options{DataDir: e.dataDir})
	if err != nil {
		t.Fatalf("open %s: %v", e.dataDir, err)
	}
	defer func() { _ = app.Close() }()

	err = app.Store.SaveDevice(ctx, device.Device{
		ID: pathPeerID, Name: "nas", Address: address, Verified: true, AddedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("save peer: %v", err)
	}
}

// pathDocument returns a `tailscale status --json` document describing the nas
// with the given current endpoint and relay region. Either may be empty, which
// is a client that cannot say which path it uses.
func pathDocument(curAddr, relay string) string {
	return fmt.Sprintf(`{
  "Version": "1.80.2",
  "BackendState": "Running",
  "Self": {
    "ID": "nSELF1CNTRL", "HostName": "laptop", "DNSName": "laptop.tail1234.ts.net.",
    "OS": "linux", "TailscaleIPs": ["100.64.0.2"], "Online": true
  },
  "MagicDNSSuffix": "tail1234.ts.net",
  "Peer": {
    "nodekey:aa": {
      "ID": "nNAS123CNTRL", "HostName": "nas", "DNSName": "nas.tail1234.ts.net.",
      "OS": "linux", "TailscaleIPs": ["100.64.0.3"], "Online": true, "Active": true,
      "CurAddr": %q, "Relay": %q
    }
  }
}`, curAddr, relay)
}

// runPath runs `network path --json` and decodes it.
func runPath(t *testing.T, e env) []peerPathReport {
	t.Helper()
	code, stdout, stderr := e.run("", "network", "path", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got []peerPathReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	return got
}

func TestNetworkPath_DirectPeer(t *testing.T) {
	stubTailscale(t, pathDocument("192.168.1.20:41641", ""))
	e := newEnv(t)
	e.init(t)
	savePathPeer(t, e, "100.64.0.3:22000")

	got := runPath(t, e)
	if len(got) != 1 {
		t.Fatalf("paths = %+v, want one row per configured peer", got)
	}
	if got[0].Kind != "direct" || got[0].Endpoint != "192.168.1.20:41641" {
		t.Fatalf("path = %+v, want the direct endpoint the client named", got[0])
	}
	if got[0].Reason != nil {
		t.Errorf("reason = %q, want null on a path that is known", *got[0].Reason)
	}

	code, stdout, _ := e.run("", "network", "path")
	if code != cli.ExitOK {
		t.Fatalf("text exit = %d, want 0", code)
	}
	for _, want := range []string{"PEER", "PATH", "direct", "192.168.1.20:41641"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output %q does not contain %q", stdout, want)
		}
	}
}

func TestNetworkPath_RelayedPeer(t *testing.T) {
	stubTailscale(t, pathDocument("", "fra"))
	e := newEnv(t)
	e.init(t)
	savePathPeer(t, e, "nas.tail1234.ts.net:22000")

	got := runPath(t, e)
	if got[0].Kind != "relay" || got[0].Relay != "fra" {
		t.Fatalf("path = %+v, want the DERP region the client named", got[0])
	}

	code, stdout, _ := e.run("", "network", "path")
	if code != cli.ExitOK {
		t.Fatalf("text exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "DERP fra") {
		t.Errorf("text output %q does not name the relay", stdout)
	}
}

func TestNetworkPath_UnknownIsNeverGuessed(t *testing.T) {
	stubTailscale(t, pathDocument("", ""))
	e := newEnv(t)
	e.init(t)
	savePathPeer(t, e, "100.64.0.3:22000")

	got := runPath(t, e)
	switch {
	case got[0].Kind != "unknown":
		t.Fatalf("path = %+v, want unknown when the client names neither endpoint nor relay", got[0])
	case got[0].Reason == nil:
		t.Fatal("an unknown path carries no reason")
	case got[0].Endpoint != "" || got[0].Relay != "":
		t.Fatalf("path = %+v, want nothing invented", got[0])
	}
}

func TestNetworkPath_PeerThatIsNotATailnetNode(t *testing.T) {
	stubTailscale(t, pathDocument("192.168.1.20:41641", ""))
	e := newEnv(t)
	e.init(t)
	savePathPeer(t, e, "10.0.0.7:22000")

	got := runPath(t, e)
	if got[0].Kind != "unknown" || got[0].Reason == nil {
		t.Fatalf("path = %+v, want unknown with a reason for an address the tailnet does not know", got[0])
	}
	if !strings.Contains(*got[0].Reason, "10.0.0.7") {
		t.Errorf("reason = %q, does not name the address that was looked up", *got[0].Reason)
	}
}

func TestNetworkPath_AbsentClientExits11(t *testing.T) {
	withoutTailscale(t)
	e := newEnv(t)
	e.init(t)
	savePathPeer(t, e, "100.64.0.3:22000")

	code, _, stderr := e.run("", "network", "path")
	if code != cli.ExitTailnetUnavailable {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitTailnetUnavailable, stderr)
	}
}
