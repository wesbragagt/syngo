package runner

import (
	"strings"
	"testing"
)

// tailscaleStatusJSON is the shape `tailscale status --json` prints: one peer
// holding a direct endpoint, one falling back to a relay.
const tailscaleStatusJSON = `{
  "Self": {"HostName": "local-host", "DNSName": "local-host.tail1234.ts.net."},
  "Peer": {
    "nodekey:aaa": {"HostName": "peer-host", "DNSName": "peer-host.tail1234.ts.net.",
                    "CurAddr": "100.64.0.7:41641", "Relay": "ord", "Active": true},
    "nodekey:bbb": {"HostName": "phone", "DNSName": "phone.tail1234.ts.net.",
                    "CurAddr": "", "Relay": "sfo", "Active": false}
  }
}`

func TestParseTailscalePeerPathFindsAPeerByName(t *testing.T) {
	for _, name := range []string{"peer-host", "PEER-HOST", "peer-host.tail1234.ts.net"} {
		got, err := parseTailscalePeerPath([]byte(tailscaleStatusJSON), name)
		if err != nil {
			t.Fatalf("parseTailscalePeerPath(%q): %v", name, err)
		}
		if got.CurAddr != "100.64.0.7:41641" {
			t.Errorf("CurAddr for %q = %q, want the direct endpoint", name, got.CurAddr)
		}
	}
}

func TestParseTailscalePeerPathRefusesAnUnknownPeer(t *testing.T) {
	if _, err := parseTailscalePeerPath([]byte(tailscaleStatusJSON), "nowhere"); err == nil {
		t.Fatal("parseTailscalePeerPath of an unknown peer = nil, want an error")
	}
	if _, err := parseTailscalePeerPath([]byte("not json"), "peer-host"); err == nil {
		t.Fatal("parseTailscalePeerPath of garbage = nil, want an error")
	}
}

// A path is only "direct" when both machines saw a direct endpoint while the
// transfer ran. One side alone proves nothing.
func TestReconcilePathsNeedsBothSides(t *testing.T) {
	cases := []struct {
		name          string
		local, remote peerPath
		wantPath      string
		wantReason    string
	}{
		{"both direct", peerPath{CurAddr: "a"}, peerPath{CurAddr: "b"}, NetworkPathDirect, ""},
		{"both relayed", peerPath{Relay: "ord"}, peerPath{Relay: "ord"}, "relay:ord", ""},
		{"only one direct", peerPath{CurAddr: "a"}, peerPath{Relay: "ord"}, "", "disagree"},
		{"nothing at all", peerPath{}, peerPath{}, "", "neither machine"},
	}
	for _, c := range cases {
		got := reconcilePaths(c.local, c.remote)
		if got.Path != c.wantPath {
			t.Errorf("%s: path = %q, want %q", c.name, got.Path, c.wantPath)
		}
		if c.wantPath == "" && !strings.Contains(got.Reason, c.wantReason) {
			t.Errorf("%s: reason = %q, want it to mention %q", c.name, got.Reason, c.wantReason)
		}
	}
}

func TestGoPlatformMapsWhatThePeerReports(t *testing.T) {
	cases := map[string]string{
		"Linux x86_64\n":  "linux/amd64",
		"Linux aarch64":   "linux/arm64",
		"Darwin arm64":    "darwin/arm64",
		"Darwin x86_64\n": "darwin/amd64",
	}
	for in, want := range cases {
		goos, goarch, err := goPlatform(in)
		if err != nil {
			t.Errorf("goPlatform(%q): %v", in, err)
			continue
		}
		if goos+"/"+goarch != want {
			t.Errorf("goPlatform(%q) = %s/%s, want %s", in, goos, goarch, want)
		}
	}
	for _, in := range []string{"", "Linux", "Plan9 x86_64", "Linux sparc"} {
		if _, _, err := goPlatform(in); err == nil {
			t.Errorf("goPlatform(%q) = nil, want an error", in)
		}
	}
}
