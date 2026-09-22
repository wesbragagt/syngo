package tailscalecli

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// binaryName is the executable asked for on every call.
const binaryName = "tailscale"

// commandTimeout bounds one `tailscale status --json` run. The local client
// answers from its own state, so a slower answer than this means the daemon is
// wedged and waiting longer would only hold a CLI command up.
const commandTimeout = 5 * time.Second

// tailscaleRange is the CGNAT range Tailscale hands out. It is how the
// interface is recognized when the JSON does not name one, which it never
// does.
var tailscaleRange = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// Discovery answers what the local Tailscale client knows. The binary is
// looked up on every call rather than once at startup, so a device without
// Tailscale pays nothing and one that installs it later needs no restart.
type Discovery struct {
	runner Runner
	binary string
	// interfaces lists the network interfaces and their addresses. It is
	// localInterfaces, replaced in tests.
	interfaces func() ([]iface, error)
}

// iface is one network interface reduced to what the lookup needs: its name
// and the addresses configured on it.
type iface struct {
	name string
	ips  []net.IP
}

// New returns a discovery that runs the tailscale binary through runner.
func New(runner Runner) *Discovery {
	return &Discovery{runner: runner, binary: binaryName, interfaces: localInterfaces}
}

// localInterfaces reads the interfaces of this machine.
func localInterfaces() ([]iface, error) {
	found, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]iface, 0, len(found))
	for _, each := range found {
		addrs, err := each.Addrs()
		if err != nil {
			continue
		}
		entry := iface{name: each.Name}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				entry.ips = append(entry.ips, ipNet.IP)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// Status describes this device on its tailnet.
func (d *Discovery) Status(ctx context.Context) (ports.TailnetStatus, error) {
	status, _, err := d.fetch(ctx)
	return status, err
}

// Peers lists the other nodes of the tailnet, sorted by host name. A backend
// that is not running knows nothing current, so the listing is refused rather
// than answered from a stale network map.
func (d *Discovery) Peers(ctx context.Context) ([]ports.TailnetNode, error) {
	status, peers, err := d.fetch(ctx)
	if err != nil {
		return nil, err
	}
	if !status.Running {
		return nil, notRunning(status)
	}
	return peers, nil
}

// Resolve turns a host name or MagicDNS name into tailnet addresses. Self is
// included, so a caller that names its own device is told so by whoever
// compares the identity afterwards.
func (d *Discovery) Resolve(ctx context.Context, name string) ([]string, error) {
	status, peers, err := d.fetch(ctx)
	if err != nil {
		return nil, err
	}
	if !status.Running {
		return nil, notRunning(status)
	}
	for _, node := range append([]ports.TailnetNode{status.Self}, peers...) {
		if matches(node, name, status.MagicDNSSuffix) {
			if len(node.IPs) == 0 {
				continue
			}
			return node.IPs, nil
		}
	}
	return nil, fmt.Errorf("%w: no tailnet node named %q", ports.ErrNotFound, name)
}

// Paths reports how traffic reaches each other node of the tailnet. A backend
// that is not running has no paths to describe, so the listing is refused
// rather than answered from a stale network map.
func (d *Discovery) Paths(ctx context.Context) ([]ports.TailnetPath, error) {
	raw, err := d.status(ctx)
	if err != nil {
		return nil, err
	}
	status, _, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if !status.Running {
		return nil, notRunning(status)
	}
	return ParsePaths(raw)
}

// status runs `tailscale status --json` once and returns what it printed.
func (d *Discovery) status(ctx context.Context) ([]byte, error) {
	path, err := d.runner.LookPath(d.binary)
	if err != nil {
		return nil, ports.ErrTailnetNotInstalled
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	raw, err := d.runner.Run(ctx, path, "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("%w: %s status --json: %v",
			ports.ErrTailnetUnavailable, d.binary, err)
	}
	return raw, nil
}

// fetch runs the status command once and parses it.
func (d *Discovery) fetch(ctx context.Context) (ports.TailnetStatus, []ports.TailnetNode, error) {
	raw, err := d.status(ctx)
	if err != nil {
		return ports.TailnetStatus{}, nil, err
	}

	status, peers, err := Parse(raw)
	if err != nil {
		return ports.TailnetStatus{}, nil, err
	}
	status.Interface = d.findInterface(status.Self.IPs)
	return status, peers, nil
}

// notRunning reports a backend that cannot answer, naming the state so the
// user knows whether to log in, start it, or install it.
func notRunning(status ports.TailnetStatus) error {
	state := status.BackendState
	if state == "" {
		state = "unknown"
	}
	return fmt.Errorf("%w: backend state %s", ports.ErrTailnetUnavailable, state)
}

// matches reports whether a node answers to the given name, case-insensitively:
// its host name, its MagicDNS name, or its host name completed with the
// tailnet suffix.
func matches(node ports.TailnetNode, name, suffix string) bool {
	wanted := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if wanted == "" {
		return false
	}
	candidates := []string{node.HostName, node.DNSName}
	if suffix != "" && node.HostName != "" {
		candidates = append(candidates, node.HostName+"."+suffix)
	}
	for _, candidate := range candidates {
		if candidate != "" && strings.EqualFold(candidate, wanted) {
			return true
		}
	}
	return false
}

// findInterface returns the name of the interface carrying one of this
// device's tailnet addresses, or the first one holding a Tailscale address at
// all. It is empty when Tailscale runs in userspace networking mode, where
// there is no interface to name.
func (d *Discovery) findInterface(selfIPs []string) string {
	ifaces, err := d.interfaces()
	if err != nil {
		return ""
	}
	wanted := make(map[string]struct{}, len(selfIPs))
	for _, ip := range selfIPs {
		wanted[ip] = struct{}{}
	}

	fallback := ""
	for _, each := range ifaces {
		for _, ip := range each.ips {
			if _, self := wanted[ip.String()]; self {
				return each.name
			}
			if v4 := ip.To4(); v4 != nil && tailscaleRange.Contains(v4) && fallback == "" {
				fallback = each.name
			}
		}
	}
	return fallback
}
