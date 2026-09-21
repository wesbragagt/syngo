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

// Resolves reports how many times Resolve was called.
func (t *Tailnet) Resolves() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resolves
}
