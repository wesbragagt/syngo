package tailscalecli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
)

// fakeRunner stands in for the tailscale binary.
type fakeRunner struct {
	// missing makes LookPath fail, as it does without an installation.
	missing bool
	out     []byte
	err     error
	// calls records the arguments of each Run.
	calls [][]string
	// deadline is the deadline of the context the adapter passed.
	deadline time.Time
	hasDead  bool
	// block makes Run wait for the context instead of answering.
	block bool
}

func (r *fakeRunner) LookPath(file string) (string, error) {
	if r.missing {
		return "", errors.New("exec: \"" + file + "\": executable file not found in $PATH")
	}
	return "/usr/bin/" + file, nil
}

func (r *fakeRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{path}, args...))
	r.deadline, r.hasDead = ctx.Deadline()
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.out, r.err
}

// load reads one of the recorded status documents.
func load(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

// discovery returns a Discovery over the runner, with no interfaces so the
// lookup does not depend on the machine running the test.
func discovery(runner Runner) *Discovery {
	d := New(runner)
	d.interfaces = func() ([]iface, error) { return nil, nil }
	return d
}

func TestStatus_BinaryMissingIsUnavailable(t *testing.T) {
	d := discovery(&fakeRunner{missing: true})

	_, err := d.Status(context.Background())
	if !errors.Is(err, ports.ErrTailnetNotInstalled) {
		t.Fatalf("error = %v, want ErrTailnetNotInstalled", err)
	}
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want a missing binary to be unavailable too", err)
	}
	if got := err.Error(); got != "tailscale is not available: not installed" {
		t.Fatalf("error = %q, want it to say the binary is missing", got)
	}
}

func TestStatus_AFailingBinaryIsUnavailableButInstalled(t *testing.T) {
	// The difference matters to `network tailscale detect`, which reports
	// a client that is there and broken differently from one that is not
	// there at all.
	for _, runner := range []Runner{
		&fakeRunner{err: errors.New("exit status 1")},
		&fakeRunner{out: []byte("this is not json")},
	} {
		_, err := discovery(runner).Status(context.Background())
		if !errors.Is(err, ports.ErrTailnetUnavailable) {
			t.Fatalf("error = %v, want ErrTailnetUnavailable", err)
		}
		if errors.Is(err, ports.ErrTailnetNotInstalled) {
			t.Fatalf("error = %v, want it not to claim the binary is missing", err)
		}
	}
}

func TestStatus_CommandFailureIsUnavailableWithStderr(t *testing.T) {
	d := discovery(&fakeRunner{err: errors.New("exit status 1: failed to connect to local tailscaled")})

	_, err := d.Status(context.Background())
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want ErrTailnetUnavailable", err)
	}
	if got := err.Error(); !strings.Contains(got, "failed to connect to local tailscaled") {
		t.Fatalf("error = %q, want it to carry what tailscale printed", got)
	}
}

func TestStatus_RunsStatusJSON(t *testing.T) {
	runner := &fakeRunner{out: load(t, "status_current.json")}
	d := discovery(runner)

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Running || status.Self.HostName != "laptop" {
		t.Fatalf("status = %+v, want the fixture read", status)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %v, want exactly one", runner.calls)
	}
	call := runner.calls[0]
	if len(call) != 3 || call[1] != "status" || call[2] != "--json" {
		t.Fatalf("call = %v, want `tailscale status --json`", call)
	}
}

func TestPeers_NotRunningIsUnavailable(t *testing.T) {
	d := discovery(&fakeRunner{out: load(t, "status_stopped.json")})

	_, err := d.Peers(context.Background())
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want ErrTailnetUnavailable", err)
	}
	if got := err.Error(); !strings.Contains(got, "Stopped") {
		t.Fatalf("error = %q, want it to name the backend state", got)
	}

	// Status itself still answers: "stopped" is the answer detection wants.
	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Running {
		t.Fatalf("status = %+v, want not running", status)
	}
}

func TestPeers_SortedWithoutSelf(t *testing.T) {
	d := discovery(&fakeRunner{out: load(t, "status_current.json")})

	peers, err := d.Peers(context.Background())
	if err != nil {
		t.Fatalf("peers: %v", err)
	}
	if len(peers) != 3 || peers[0].HostName != "desktop" || peers[2].HostName != "phone" {
		t.Fatalf("peers = %+v, want three sorted by host name", peers)
	}
}

func TestResolve_BareHostnameFQDNAndCaseInsensitive(t *testing.T) {
	d := discovery(&fakeRunner{out: load(t, "status_current.json")})

	for _, name := range []string{"nas", "NAS", "nas.tail1234.ts.net", "nas.tail1234.ts.net.", " nas "} {
		ips, err := d.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %q: %v", name, err)
		}
		if len(ips) != 2 || ips[0] != "100.64.0.3" {
			t.Fatalf("resolve %q = %v, want the IPv4 address first", name, ips)
		}
	}
}

func TestResolve_SelfResolves(t *testing.T) {
	d := discovery(&fakeRunner{out: load(t, "status_current.json")})

	ips, err := d.Resolve(context.Background(), "laptop")
	if err != nil {
		t.Fatalf("resolve self: %v", err)
	}
	if len(ips) == 0 || ips[0] != "100.64.0.2" {
		t.Fatalf("resolve self = %v, want this device's own address", ips)
	}
}

func TestResolve_UnknownIsNotFound(t *testing.T) {
	d := discovery(&fakeRunner{out: load(t, "status_current.json")})

	_, err := d.Resolve(context.Background(), "unknown-host")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestResolve_NotRunningIsUnavailable(t *testing.T) {
	d := discovery(&fakeRunner{out: load(t, "status_stopped.json")})

	_, err := d.Resolve(context.Background(), "nas")
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want ErrTailnetUnavailable", err)
	}
}

func TestStatus_InterfaceFoundByAddress(t *testing.T) {
	d := New(&fakeRunner{out: load(t, "status_current.json")})
	d.interfaces = func() ([]iface, error) {
		return []iface{
			{name: "lo", ips: []net.IP{net.ParseIP("127.0.0.1")}},
			{name: "eth0", ips: []net.IP{net.ParseIP("192.168.1.20")}},
			{name: "tailscale0", ips: []net.IP{net.ParseIP("100.64.0.2")}},
		}, nil
	}

	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Interface != "tailscale0" {
		t.Fatalf("interface = %q, want tailscale0", status.Interface)
	}
}

func TestStatus_InterfaceFallsBackToTheTailscaleRangeThenEmpty(t *testing.T) {
	d := New(&fakeRunner{out: load(t, "status_current.json")})
	// A device whose addresses moved since the status was taken: the
	// CGNAT range still identifies the interface.
	d.interfaces = func() ([]iface, error) {
		return []iface{
			{name: "eth0", ips: []net.IP{net.ParseIP("192.168.1.20")}},
			{name: "ts0", ips: []net.IP{net.ParseIP("100.100.5.5")}},
		}, nil
	}
	status, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Interface != "ts0" {
		t.Fatalf("interface = %q, want ts0", status.Interface)
	}

	// Userspace networking: no interface carries a tailnet address.
	d.interfaces = func() ([]iface, error) {
		return []iface{{name: "eth0", ips: []net.IP{net.ParseIP("192.168.1.20")}}}, nil
	}
	status, err = d.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Interface != "" {
		t.Fatalf("interface = %q, want empty for userspace networking", status.Interface)
	}
}

func TestRun_HonoursTimeout(t *testing.T) {
	runner := &fakeRunner{block: true}
	d := discovery(runner)

	// The parent context is cancelled at once: the assertion is about the
	// deadline the adapter put on top of it, not about waiting it out.
	ctx, cancel := context.WithCancel(context.Background())
	before := time.Now()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := d.Status(ctx)
	defer cancel()
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want ErrTailnetUnavailable", err)
	}
	if !runner.hasDead {
		t.Fatalf("the adapter passed a context without a deadline")
	}
	if left := runner.deadline.Sub(before); left <= 0 || left > commandTimeout+time.Second {
		t.Fatalf("deadline in %v, want about %v", left, commandTimeout)
	}
}
