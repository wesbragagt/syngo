package ports

import (
	"context"
	"errors"
	"fmt"
)

// ErrTailnetUnavailable reports that the local Tailscale client could not be
// asked: the binary is missing, the command failed, or the backend is not
// running. It is not a failure of the thing that was asked about.
var ErrTailnetUnavailable = errors.New("tailscale is not available")

// ErrTailnetNotInstalled reports the one case of ErrTailnetUnavailable that
// says nothing is wrong: there is no Tailscale binary on this device. Every
// other unavailability means a client that is there and cannot answer, which
// callers report differently.
var ErrTailnetNotInstalled = fmt.Errorf("%w: not installed", ErrTailnetUnavailable)

// TailnetNode is one device the local Tailscale client knows about.
type TailnetNode struct {
	// ID is the stable Tailscale node id, such as "nNAS123CNTRL".
	ID string
	// HostName is the short name, such as "nas".
	HostName string
	// DNSName is the MagicDNS name without its trailing dot, such as
	// "nas.tail1234.ts.net".
	DNSName string
	// IPs are the tailnet addresses of the node, IPv4 first.
	IPs []string
	// Online reports whether Tailscale considers the node reachable.
	Online bool
	// OS is what the node reports about itself, such as "linux".
	OS string
}

// TailnetStatus is what the local Tailscale client says about this device.
type TailnetStatus struct {
	// Version is the Tailscale client version string.
	Version string
	// BackendState is "Running", "Stopped", "NeedsLogin" and so on.
	BackendState string
	// Running reports BackendState == "Running".
	Running bool
	// Interface carries this device's tailnet address, or is empty when
	// Tailscale runs in userspace networking mode.
	Interface string
	// MagicDNSSuffix is the tailnet's DNS suffix, such as "tail1234.ts.net".
	MagicDNSSuffix string
	// Self is this device as the tailnet sees it.
	Self TailnetNode
}

// TailnetDiscovery answers what the local Tailscale client knows. Every method
// reports ErrTailnetUnavailable when there is no client to ask, so a device
// without Tailscale keeps working with explicit addresses.
type TailnetDiscovery interface {
	// Status describes this device on its tailnet.
	Status(ctx context.Context) (TailnetStatus, error)
	// Peers lists the other nodes of the tailnet, sorted by host name. It
	// reports ErrTailnetUnavailable when the backend is not running.
	Peers(ctx context.Context) ([]TailnetNode, error)
	// Resolve turns a host name or MagicDNS name into tailnet addresses,
	// IPv4 first. It reports ErrNotFound when the tailnet has no such node.
	Resolve(ctx context.Context, name string) ([]string, error)
}
