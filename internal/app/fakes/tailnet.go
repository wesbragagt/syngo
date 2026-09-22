package fakes

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// Tailnet is an in-memory ports.TailnetDiscovery. It answers from a fixed
// node list and counts the calls, so a test can prove that an address that
// needs no resolution never asks.
type Tailnet struct {
	mu     sync.Mutex
	status ports.TailnetStatus
	peers  []ports.TailnetNode
	// err is returned by every method; ports.ErrTailnetUnavailable makes
	// the fake behave as a device without Tailscale.
	err      error
	resolves int
	// paths is what Paths reports; nil means every peer is reported as
	// unknown, which is what a client that says nothing looks like.
	paths []ports.TailnetPath
}

// NewTailnet returns a running tailnet with the given peers, self named
// "self" with the suffix "tail1234.ts.net".
func NewTailnet(peers ...ports.TailnetNode) *Tailnet {
	return &Tailnet{
		status: ports.TailnetStatus{
			Version:        "1.80.2",
			BackendState:   "Running",
			Running:        true,
			Interface:      "tailscale0",
			MagicDNSSuffix: "tail1234.ts.net",
			Self: ports.TailnetNode{
				ID: "nSELF", HostName: "self", DNSName: "self.tail1234.ts.net",
				IPs: []string{"100.64.0.2"}, Online: true, OS: "linux",
			},
		},
		peers: peers,
	}
}

// NewTailnetUnavailable returns a tailnet that behaves as a device without
// Tailscale installed.
func NewTailnetUnavailable() *Tailnet {
	return &Tailnet{err: ports.ErrTailnetNotInstalled}
}

// TailnetNode is a convenience for building a node with one IPv4 address.
func TailnetNode(host, ip string) ports.TailnetNode {
	return ports.TailnetNode{
		ID: "n" + host, HostName: host, DNSName: host + ".tail1234.ts.net",
		IPs: []string{ip}, Online: true, OS: "linux",
	}
}

// SetStatus replaces what Status reports.
func (t *Tailnet) SetStatus(s ports.TailnetStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = s
}

// SetError makes every method fail with err; nil clears it.
func (t *Tailnet) SetError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = err
}

// Status returns the configured status.
func (t *Tailnet) Status(context.Context) (ports.TailnetStatus, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return ports.TailnetStatus{}, t.err
	}
	return t.status, nil
}

// Peers returns the configured nodes, sorted by host name.
func (t *Tailnet) Peers(context.Context) ([]ports.TailnetNode, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	out := append([]ports.TailnetNode(nil), t.peers...)
	sort.Slice(out, func(i, j int) bool { return out[i].HostName < out[j].HostName })
	return out, nil
}

// Resolve matches a host name or MagicDNS name against the nodes and self.
func (t *Tailnet) Resolve(_ context.Context, name string) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resolves++
	if t.err != nil {
		return nil, t.err
	}
	for _, node := range append([]ports.TailnetNode{t.status.Self}, t.peers...) {
		if node.HostName == name || node.DNSName == name {
			return node.IPs, nil
		}
	}
	return nil, fmt.Errorf("%w: no tailnet node named %q", ports.ErrNotFound, name)
}

// SetPaths replaces what Paths reports.
func (t *Tailnet) SetPaths(paths ...ports.TailnetPath) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paths = paths
}

// TailnetDirectPath is a convenience for a node reached peer-to-peer.
func TailnetDirectPath(node ports.TailnetNode, endpoint string) ports.TailnetPath {
	return ports.TailnetPath{Node: node, Kind: ports.TailnetPathDirect, Endpoint: endpoint, Active: true}
}

// TailnetRelayPath is a convenience for a node reached through a DERP relay.
func TailnetRelayPath(node ports.TailnetNode, region string) ports.TailnetPath {
	return ports.TailnetPath{Node: node, Kind: ports.TailnetPathRelay, Relay: region, Active: true}
}

// Paths returns the configured paths, or one unknown path per peer.
func (t *Tailnet) Paths(context.Context) ([]ports.TailnetPath, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	if t.paths != nil {
		return append([]ports.TailnetPath(nil), t.paths...), nil
	}
	out := make([]ports.TailnetPath, 0, len(t.peers))
	for _, peer := range t.peers {
		out = append(out, ports.TailnetPath{
			Node: peer, Kind: ports.TailnetPathUnknown,
			Reason: "the fake tailnet was not told which path this node uses",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node.HostName < out[j].Node.HostName })
	return out, nil
}

// Resolves reports how many times Resolve was called.
func (t *Tailnet) Resolves() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resolves
}
