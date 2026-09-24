package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/cli"
)

// detectReport is the JSON shape of `network tailscale detect`.
type detectReport struct {
	Present        bool     `json:"present"`
	Running        bool     `json:"running"`
	BackendState   string   `json:"backend_state"`
	Version        string   `json:"version"`
	Interface      string   `json:"interface"`
	HostName       string   `json:"hostname"`
	DNSName        string   `json:"dns_name"`
	IPs            []string `json:"ips"`
	MagicDNSSuffix string   `json:"magic_dns_suffix"`
	Error          *string  `json:"error"`
}

// tailnetPeerReport is the JSON shape of one node in `network peers`.
type tailnetPeerReport struct {
	ID       string   `json:"id"`
	HostName string   `json:"hostname"`
	DNSName  string   `json:"dns_name"`
	IPs      []string `json:"ips"`
	Online   bool     `json:"online"`
	OS       string   `json:"os"`
}

// withoutTailscale empties PATH for the test, so the adapter finds no
// tailscale binary however the machine running the tests is set up.
func withoutTailscale(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// stubTailscale puts an executable named tailscale on PATH that prints the
// given `tailscale status --json` document. It is how the CLI tests exercise
// the present path on a machine that may have no tailnet.
func stubTailscale(t *testing.T, document string) {
	t.Helper()
	// The stub runs with PATH pointing at its own directory, so the
	// command it uses to print the document is named in full.
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat on PATH to write a stub tailscale with")
	}

	dir := t.TempDir()
	status := filepath.Join(dir, "status.json")
	if err := os.WriteFile(status, []byte(document), 0o600); err != nil {
		t.Fatalf("write the stub status: %v", err)
	}
	stubTailscaleScript(t, dir, fmt.Sprintf("%s %s\n", cat, status))
}

// stubTailscaleScript puts an executable named tailscale in dir whose body is
// the given shell script, and points PATH at it. It is how the tests exercise
// a client that is installed and cannot answer.
func stubTailscaleScript(t *testing.T, dir, body string) {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH to write a stub tailscale with")
	}
	script := "#!" + shell + "\n" + body
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the stub binary: %v", err)
	}
	t.Setenv("PATH", dir)
}

// tailnetDocument returns a `tailscale status --json` document naming one peer
// at the given address, in the shape current clients print.
func tailnetDocument(peerHost, peerIP string) string {
	return fmt.Sprintf(`{
  "Version": "1.80.2",
  "BackendState": "Running",
  "TailscaleIPs": ["100.64.0.2"],
  "Self": {
    "ID": "nSELF1CNTRL", "HostName": "laptop", "DNSName": "laptop.tail1234.ts.net.",
    "OS": "linux", "TailscaleIPs": ["100.64.0.2", "fd7a:115c:a1e0::2"], "Online": true
  },
  "MagicDNSSuffix": "tail1234.ts.net",
  "CurrentTailnet": {"Name": "example@github", "MagicDNSSuffix": "tail1234.ts.net", "MagicDNSEnabled": true},
  "Peer": {
    "nodekey:aa": {
      "ID": "nNAS123CNTRL", "HostName": %q, "DNSName": %q,
      "OS": "linux", "TailscaleIPs": [%q], "Online": true
    },
    "nodekey:bb": {
      "ID": "nOFF123CNTRL", "HostName": "attic", "DNSName": "attic.tail1234.ts.net.",
      "OS": "windows", "TailscaleIPs": ["100.64.0.9"], "Online": false
    }
  }
}`, peerHost, peerHost+".tail1234.ts.net.", peerIP)
}

func TestNetworkTailscaleDetect_AbsentPrintsPresentFalse(t *testing.T) {
	withoutTailscale(t)
	e := newEnv(t)
	e.init(t)

	code, stdout, stderr := e.run("", "network", "tailscale", "detect", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got detectReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	if got.Present || got.Running || got.BackendState != "" || got.Version != "" {
		t.Fatalf("detection = %+v, want an absent client", got)
	}
	if got.IPs == nil || len(got.IPs) != 0 {
		t.Fatalf("ips = %v, want an empty array", got.IPs)
	}

	code, stdout, _ = e.run("", "network", "tailscale", "detect")
	if code != cli.ExitOK || !strings.Contains(stdout, "tailscale: not installed") {
		t.Fatalf("text output = %q (exit %d), want it to say Tailscale is not installed", stdout, code)
	}
}

func TestNetworkPeers_AbsentExit11(t *testing.T) {
	withoutTailscale(t)
	e := newEnv(t)
	e.init(t)

	code, _, stderr := e.run("", "network", "peers")
	if code != cli.ExitTailnetUnavailable {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitTailnetUnavailable, stderr)
	}
	if !strings.Contains(stderr, "not installed") {
		t.Fatalf("stderr = %q, want it to say the binary is missing", stderr)
	}
}

func TestNetworkTailscaleDetect_StubBinaryReadsFixture(t *testing.T) {
	stubTailscale(t, tailnetDocument("nas", "100.64.0.3"))
	e := newEnv(t)
	e.init(t)

	code, stdout, stderr := e.run("", "network", "tailscale", "detect", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got detectReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	if !got.Present || !got.Running || got.BackendState != "Running" || got.Version != "1.80.2" {
		t.Fatalf("detection = %+v, want a running client", got)
	}
	if got.HostName != "laptop" || got.DNSName != "laptop.tail1234.ts.net" ||
		got.MagicDNSSuffix != "tail1234.ts.net" {
		t.Fatalf("detection = %+v, want self read from the document", got)
	}
	if len(got.IPs) != 2 || got.IPs[0] != "100.64.0.2" {
		t.Fatalf("ips = %v, want the IPv4 address first", got.IPs)
	}
}

func TestNetworkPeers_StubBinaryTable(t *testing.T) {
	stubTailscale(t, tailnetDocument("nas", "100.64.0.3"))
	e := newEnv(t)
	e.init(t)

	code, stdout, stderr := e.run("", "network", "peers", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got []tailnetPeerReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	if len(got) != 2 || got[0].HostName != "attic" || got[1].HostName != "nas" {
		t.Fatalf("peers = %+v, want attic and nas in that order", got)
	}
	if got[0].Online || !got[1].Online {
		t.Fatalf("peers = %+v, want the offline node reported as offline", got)
	}
	if got[1].DNSName != "nas.tail1234.ts.net" || len(got[1].IPs) != 1 || got[1].IPs[0] != "100.64.0.3" {
		t.Fatalf("nas = %+v, want its MagicDNS name without a trailing dot", got[1])
	}

	code, stdout, _ = e.run("", "network", "peers")
	if code != cli.ExitOK {
		t.Fatalf("text exit = %d, want 0", code)
	}
	for _, want := range []string{"HOSTNAME", "DNS NAME", "ONLINE", "nas.tail1234.ts.net", "100.64.0.3"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("table = %q, want it to contain %q", stdout, want)
		}
	}
}

func TestPeerAdd_BareHostnameResolvedThroughStubTailscale(t *testing.T) {
	// The peer runs on loopback, so the stub tailnet says "nas" is there.
	peer, addr := startPeerDaemon(t, "nas-device")
	host, port, ok := strings.Cut(addr, ":")
	if !ok || host != "127.0.0.1" {
		t.Fatalf("peer address = %q, want a loopback address", addr)
	}
	stubTailscale(t, tailnetDocument("nas", "127.0.0.1"))

	e := newEnv(t)
	e.init(t)
	code, stdout, stderr := e.run("", "peer", "add", "nas:"+port, "--yes", "--json")
	if code != cli.ExitOK {
		t.Fatalf("peer add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var added struct {
		ID           string `json:"id"`
		Address      string `json:"address"`
		ResolvedFrom string `json:"resolved_from"`
	}
	if err := json.Unmarshal([]byte(stdout), &added); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	if added.Address != addr {
		t.Fatalf("address = %q, want the resolved %q", added.Address, addr)
	}
	if added.ResolvedFrom != "nas" {
		t.Fatalf("resolved_from = %q, want nas", added.ResolvedFrom)
	}
	if added.ID != peerReport(t, peer).Device.ID {
		t.Fatalf("id = %q, want the peer's device id", added.ID)
	}

	// The text form names the host the tailnet resolved. The peer has not
	// added this device back, so the test reports it is not accepted yet,
	// which is exit 7 and not what this case is about.
	code, stdout, stderr = e.run("", "peer", "test", "nas:"+port)
	if code != cli.ExitPeerUnreachable {
		t.Fatalf("peer test exit = %d, want %d (stderr: %s)", code, cli.ExitPeerUnreachable, stderr)
	}
	if !strings.Contains(stdout, "(nas via tailnet)") {
		t.Fatalf("peer test output = %q, want it to name the tailnet resolution", stdout)
	}
}

func TestPeerAdd_UnknownHostnameFallsBackToSystemDNS(t *testing.T) {
	stubTailscale(t, tailnetDocument("nas", "100.64.0.3"))
	e := newEnv(t)
	e.init(t)

	// "attic-spare" is not on the stub tailnet, so the host is kept as
	// typed and the dialer is left to resolve it, which it cannot: the
	// address reported is the one the user gave.
	_, port, _ := strings.Cut(closedPort(t), ":")
	code, stdout, stderr := e.run("", "peer", "add", "attic-spare.invalid:"+port, "--yes")
	if code != cli.ExitPeerUnreachable {
		t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)",
			code, cli.ExitPeerUnreachable, stdout, stderr)
	}
	if !strings.Contains(stderr, "attic-spare.invalid:"+port) {
		t.Fatalf("stderr = %q, want the address as typed", stderr)
	}
}

// brokenDetection runs `network tailscale detect --json` against a stub
// tailscale whose body is the given script, checks that it reported an
// installed client that could not answer, and returns both the report and the
// environment so the caller can ask the same stub again.
func brokenDetection(t *testing.T, body string) (detectReport, env) {
	t.Helper()
	stubTailscaleScript(t, t.TempDir(), body)
	e := newEnv(t)
	e.init(t)

	code, stdout, stderr := e.run("", "network", "tailscale", "detect", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var got detectReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not json: %v\n%s", err, stdout)
	}
	if !got.Present {
		t.Fatalf("detection = %+v, want the installed binary reported as present", got)
	}
	if got.Error == nil || *got.Error == "" {
		t.Fatalf("detection = %+v, want the reason it could not answer", got)
	}
	return got, e
}

// assertBrokenText checks the text form of a detection that could not be made.
func assertBrokenText(t *testing.T, e env) {
	t.Helper()
	code, stdout, _ := e.run("", "network", "tailscale", "detect")
	if code != cli.ExitOK {
		t.Fatalf("text exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "tailscale:        installed") || !strings.Contains(stdout, "error:") {
		t.Fatalf("text output = %q, want an installed client and the reason", stdout)
	}
}

func TestNetworkTailscaleDetect_BinaryThatFailsIsInstalledWithAnError(t *testing.T) {
	got, e := brokenDetection(t, "echo 'failed to connect to local tailscaled' >&2\nexit 1\n")
	if !strings.Contains(*got.Error, "failed to connect to local tailscaled") {
		t.Fatalf("error = %q, want the standard error of the binary", *got.Error)
	}
	assertBrokenText(t, e)
}

func TestNetworkTailscaleDetect_BinaryThatPrintsNonsenseIsInstalledWithAnError(t *testing.T) {
	got, e := brokenDetection(t, "echo 'this is not json'\n")
	if !strings.Contains(*got.Error, "reading tailscale status") {
		t.Fatalf("error = %q, want it to say the answer could not be read", *got.Error)
	}
	assertBrokenText(t, e)
}

func TestNetworkTailscaleDetect_BinaryThatHangsIsInstalledWithAnError(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the five second command timeout; run without -short")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep on PATH to hang with")
	}
	// The sleep runs in the background and is waited for, so killing the
	// shell leaves a grandchild holding the output pipe.
	// One run only: each one waits out the whole command timeout.
	got, _ := brokenDetection(t, sleep+" 60 &\nwait\n")
	if !strings.Contains(*got.Error, "context deadline exceeded") {
		t.Fatalf("error = %q, want the deadline that was hit", *got.Error)
	}
}

func TestNetworkPeers_BrokenBinaryStillExits11(t *testing.T) {
	stubTailscaleScript(t, t.TempDir(), "echo 'this is not json'\n")
	e := newEnv(t)
	e.init(t)

	code, _, stderr := e.run("", "network", "peers")
	if code != cli.ExitTailnetUnavailable {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitTailnetUnavailable, stderr)
	}
}

func TestPeerAdd_LocalhostIsNotResolvedThroughTheTailnet(t *testing.T) {
	// The stub tailnet claims a node named "localhost" at an address
	// nothing listens on. The name means this machine, so the dialer is
	// the one that resolves it and the tailnet address is never used.
	stubTailscale(t, tailnetDocument("localhost", "100.64.0.3"))
	e := newEnv(t)
	e.init(t)

	_, port, _ := strings.Cut(closedPort(t), ":")
	code, stdout, stderr := e.run("", "peer", "add", "localhost:"+port, "--yes")
	if code != cli.ExitPeerUnreachable {
		t.Fatalf("exit = %d, want %d (stdout %q, stderr %q)",
			code, cli.ExitPeerUnreachable, stdout, stderr)
	}
	if !strings.Contains(stderr, "localhost:"+port) {
		t.Fatalf("stderr = %q, want the loopback address as typed", stderr)
	}
}
