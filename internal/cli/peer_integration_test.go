package cli_test

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/cli"
)

// peerStatus is the part of `status --json` the peer tests look at.
type peerStatus struct {
	Device struct {
		ID string `json:"id"`
	} `json:"device"`
	Daemon struct {
		Running bool   `json:"running"`
		Listen  string `json:"listen"`
	} `json:"daemon"`
	Peers []struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		Address   string  `json:"address"`
		Verified  bool    `json:"verified"`
		Connected bool    `json:"connected"`
		LatencyMS float64 `json:"latency_ms"`
	} `json:"peers"`
}

// peerReport runs `status --json` and decodes the peer section.
func peerReport(t *testing.T, e env) peerStatus {
	t.Helper()
	code, stdout, stderr := e.run("", "status", "--json")
	if code != cli.ExitOK {
		t.Fatalf("status exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var report peerStatus
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("status output is not json: %v\n%s", err, stdout)
	}
	return report
}

// startPeerDaemon initializes a data directory and runs a daemon on a port the
// operating system picks, returning the environment and the bound address.
func startPeerDaemon(t *testing.T, name string) (env, string) {
	t.Helper()
	e := newEnv(t)
	if code, _, stderr := e.run("", "init", "--name", name); code != cli.ExitOK {
		t.Fatalf("init exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	// The short rescan interval is also how often the peer table is
	// re-read, so a peer added later is connected without a long wait.
	startDaemon(t, e, "--listen", "127.0.0.1:0", "--rescan-interval", "200ms")
	waitForDaemon(t, e)

	addr := peerReport(t, e).Daemon.Listen
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("daemon listen = %q, want a bound address", addr)
	}
	return e, addr
}

// closedPort returns an address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func TestPeerAdd_BothDirections_ThenTestAuthenticated(t *testing.T) {
	a, addrA := startPeerDaemon(t, "a")
	b, addrB := startPeerDaemon(t, "b")
	idA := peerReport(t, a).Device.ID
	idB := peerReport(t, b).Device.ID

	// A trusts what B presents; B has not added A yet, so it refuses the
	// connection and `peer add` says so.
	code, stdout, stderr := a.run("", "peer", "add", addrB, "--verify", "--yes")
	if code != cli.ExitOK {
		t.Fatalf("peer add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	// The output uses the dashed display form of the id.
	undashed := strings.ReplaceAll(stdout, "-", "")
	if !strings.Contains(undashed, idB) || !strings.Contains(stdout, "has not added this device yet") {
		t.Fatalf("peer add output = %q, want B's id and the pairing hint", stdout)
	}

	// B pins A's identity instead of confirming it.
	if code, _, stderr = b.run("", "peer", "add", addrA, "--device-id", idA); code != cli.ExitOK {
		t.Fatalf("peer add --device-id exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	code, stdout, stderr = a.run("", "peer", "list", "--json")
	if code != cli.ExitOK {
		t.Fatalf("peer list exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var list []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Address  string `json:"address"`
		Verified bool   `json:"verified"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("peer list output is not json: %v\n%s", err, stdout)
	}
	if len(list) != 1 || list[0].ID != idB || list[0].Address != addrB || !list[0].Verified {
		t.Fatalf("peer list = %+v, want the verified peer B at %s", list, addrB)
	}

	// Both sides now know each other, so the test connection authenticates.
	code, stdout, stderr = a.run("", "peer", "test", addrB, "--json")
	if code != cli.ExitOK {
		t.Fatalf("peer test exit = %d, want 0 (stdout: %s stderr: %s)", code, stdout, stderr)
	}
	var result struct {
		DeviceID      string  `json:"device_id"`
		Name          string  `json:"name"`
		TLSOK         bool    `json:"tls_ok"`
		Authenticated bool    `json:"authenticated"`
		LatencyMS     float64 `json:"latency_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("peer test output is not json: %v\n%s", err, stdout)
	}
	if result.DeviceID != idB || !result.TLSOK || !result.Authenticated {
		t.Fatalf("peer test = %+v, want an authenticated connection to B", result)
	}
	if result.LatencyMS <= 0 {
		t.Fatalf("latency = %v, want a measured round trip", result.LatencyMS)
	}

	// Testing by device id works too, and so does the reverse direction.
	if code, _, stderr = b.run("", "peer", "test", idA); code != cli.ExitOK {
		t.Fatalf("peer test by id exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	// The daemon dials its peers, so status reports them connected.
	deadline := time.Now().Add(convergeTimeout)
	var report peerStatus
	for time.Now().Before(deadline) {
		report = peerReport(t, a)
		if len(report.Peers) == 1 && report.Peers[0].Connected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(report.Peers) != 1 || !report.Peers[0].Connected || report.Peers[0].ID != idB {
		t.Fatalf("status peers = %+v, want B connected", report.Peers)
	}

	if code, _, stderr = a.run("", "peer", "remove", idB, "--yes"); code != cli.ExitOK {
		t.Fatalf("peer remove exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if peers := peerReport(t, a).Peers; len(peers) != 0 {
		t.Fatalf("status peers after remove = %+v, want none", peers)
	}
}

func TestPeerAdd_NoInputWithoutYesExit9(t *testing.T) {
	a, _ := startPeerDaemon(t, "a")
	_, addrB := startPeerDaemon(t, "b")

	code, _, stderr := a.run("", "peer", "add", addrB, "--no-input")
	if code != cli.ExitConfirmationRequired {
		t.Fatalf("peer add exit = %d, want %d (stderr: %s)", code, cli.ExitConfirmationRequired, stderr)
	}
	if peers := peerReport(t, a).Peers; len(peers) != 0 {
		t.Fatalf("status peers = %+v, want none after a refused confirmation", peers)
	}
}

func TestPeerAdd_DeviceIDMismatchExit7(t *testing.T) {
	a, _ := startPeerDaemon(t, "a")
	_, addrB := startPeerDaemon(t, "b")
	idA := peerReport(t, a).Device.ID

	code, _, stderr := a.run("", "peer", "add", addrB, "--device-id", idA)
	if code != cli.ExitPeerUnreachable {
		t.Fatalf("peer add exit = %d, want %d (stderr: %s)", code, cli.ExitPeerUnreachable, stderr)
	}
}

func TestPeerAdd_UnreachableExit7(t *testing.T) {
	a, _ := startPeerDaemon(t, "a")

	code, _, stderr := a.run("", "peer", "add", closedPort(t), "--yes")
	if code != cli.ExitPeerUnreachable {
		t.Fatalf("peer add exit = %d, want %d (stderr: %s)", code, cli.ExitPeerUnreachable, stderr)
	}
}

func TestPeerTest_UnreachableExit7(t *testing.T) {
	a, _ := startPeerDaemon(t, "a")
	addr := closedPort(t)

	code, stdout, _ := a.run("", "peer", "test", addr)
	if code != cli.ExitPeerUnreachable {
		t.Fatalf("peer test exit = %d, want %d", code, cli.ExitPeerUnreachable)
	}
	if !strings.Contains(stdout, "reason:        dial") {
		t.Fatalf("peer test output = %q, want the dial failure reason", stdout)
	}
}

func TestPeerTest_UnknownDeviceIDExit4(t *testing.T) {
	a, _ := startPeerDaemon(t, "a")

	unknown := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	code, _, stderr := a.run("", "peer", "test", unknown)
	if code != cli.ExitNotFound {
		t.Fatalf("peer test exit = %d, want %d (stderr: %s)", code, cli.ExitNotFound, stderr)
	}
}

func TestDaemonRun_PublicListenRefusedWithoutFlag(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, _, stderr := e.run("", "daemon", "run", "--listen", "8.8.8.8:22000")
	if code != cli.ExitUsage {
		t.Fatalf("daemon run exit = %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--allow-public") {
		t.Fatalf("stderr = %q, want the --allow-public hint", stderr)
	}
}

func TestDaemonRun_InterfaceBindsLoopback(t *testing.T) {
	if _, err := net.InterfaceByName("lo"); err != nil {
		t.Skipf("no lo interface: %v", err)
	}
	e := newEnv(t)
	e.init(t)

	startDaemon(t, e, "--listen", "0.0.0.0:0", "--interface", "lo")
	waitForDaemon(t, e)

	listen := peerReport(t, e).Daemon.Listen
	if !strings.HasPrefix(listen, "127.0.0.1:") {
		t.Fatalf("daemon listen = %q, want the loopback interface address", listen)
	}
}

func TestPeerAdd_OwnAddressExit2(t *testing.T) {
	e, addr := startPeerDaemon(t, "a")

	code, _, stderr := e.run("", "peer", "add", addr, "--yes")
	if code != cli.ExitUsage {
		t.Fatalf("peer add exit = %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "this device") {
		t.Fatalf("stderr = %q, want it to say the address is this device", stderr)
	}
	if peers := peerReport(t, e).Peers; len(peers) != 0 {
		t.Fatalf("peers = %+v, want none", peers)
	}
}

func TestPeerTest_UnparsableAddressExit2(t *testing.T) {
	e := newEnv(t)
	e.init(t)

	code, _, stderr := e.run("", "peer", "test", "foo:")
	if code != cli.ExitUsage {
		t.Fatalf("peer test exit = %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "port") {
		t.Fatalf("stderr = %q, want it to explain the address", stderr)
	}
}

func TestDaemonRun_UnknownInterfaceExit2(t *testing.T) {
	if _, err := net.InterfaceByName("nosuch0"); err == nil {
		t.Skip("this machine has an interface called nosuch0")
	}
	e := newEnv(t)
	e.init(t)

	code, _, stderr := e.run("", "daemon", "run", "--interface", "nosuch0")
	if code != cli.ExitUsage {
		t.Fatalf("daemon run exit = %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "nosuch0") {
		t.Fatalf("stderr = %q, want it to name the interface", stderr)
	}
}
