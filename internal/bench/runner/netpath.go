package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Network paths a two-machine run can record.
const (
	// NetworkPathDirect means the tailnet held a peer-to-peer connection:
	// the peer carried a non-empty CurAddr while the transfer was running.
	NetworkPathDirect = "direct"
	// NetworkPathRelay means the traffic went through a DERP relay.
	NetworkPathRelay = "relay"
)

// peerPath is what one machine's `tailscale status --json` says about the
// other machine.
type peerPath struct {
	// CurAddr is the current direct endpoint, empty when there is none.
	CurAddr string
	// Relay is the DERP region the peer falls back to.
	Relay string
	// Active reports whether traffic is flowing to the peer right now.
	Active bool
}

// parseTailscalePeerPath finds host among the peers of a `tailscale status
// --json` document. host matches a peer's HostName or the first label of its
// MagicDNS name, case-insensitively.
func parseTailscalePeerPath(raw []byte, host string) (peerPath, error) {
	var doc struct {
		Peer map[string]struct {
			HostName string `json:"HostName"`
			DNSName  string `json:"DNSName"`
			CurAddr  string `json:"CurAddr"`
			Relay    string `json:"Relay"`
			Active   bool   `json:"Active"`
		} `json:"Peer"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return peerPath{}, fmt.Errorf("read tailscale status: %w", err)
	}
	want := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, node := range doc.Peer {
		if !tailnetNameMatches(node.HostName, node.DNSName, want) {
			continue
		}
		return peerPath{CurAddr: node.CurAddr, Relay: node.Relay, Active: node.Active}, nil
	}
	return peerPath{}, fmt.Errorf("tailscale status lists no peer named %q", host)
}

// tailnetNameMatches reports whether want names the node, by host name, by
// MagicDNS name, or by the first label of it.
func tailnetNameMatches(hostName, dnsName, want string) bool {
	if want == "" {
		return false
	}
	if strings.EqualFold(hostName, want) {
		return true
	}
	dns := strings.ToLower(strings.TrimSuffix(dnsName, "."))
	if dns == "" {
		return false
	}
	return dns == want || strings.SplitN(dns, ".", 2)[0] == want
}

// networkPathObserver samples the tailnet path on both machines while a
// transfer is running. A field read after the transfer is empty and proves
// nothing, so this runs concurrently with the convergence wait and keeps the
// strongest sample it saw: a direct connection wins over a relay, because
// Tailscale starts on the relay and upgrades.
type networkPathObserver struct {
	local  func(context.Context) ([]byte, error)
	remote func(context.Context) ([]byte, error)
	// localHost and peerHost are the names each side looks the other up by.
	localHost, peerHost string
	// transferring reports whether bytes are moving between the two
	// machines right now. The receiving side is the one that knows: a
	// transfer is journalled where it lands.
	transferring func(context.Context) (bool, error)

	done   chan struct{}
	result chan networkPathSample
}

// networkPathSample is what the observer concluded.
type networkPathSample struct {
	// Path is "direct", "relay:<region>" or empty when nothing was read
	// while the connection was live.
	Path string
	// Reason explains an empty Path.
	Reason string
}

// observeNetworkPath starts sampling until stop is called. It only records a
// sample taken while a transfer is in flight, which is the only moment the
// connection is provably live.
func observeNetworkPath(ctx context.Context, o networkPathObserver, poll time.Duration) *networkPathObserver {
	o.done = make(chan struct{})
	o.result = make(chan networkPathSample, 1)
	go func() {
		o.result <- o.loop(ctx, poll)
	}()
	return &o
}

// loop samples until done is closed or a direct path is seen on both sides.
func (o *networkPathObserver) loop(ctx context.Context, poll time.Duration) networkPathSample {
	var best networkPathSample
	best.Reason = "no transfer was in flight while the tailnet status could be read, " +
		"so no sample proves which path the bytes took"
	for {
		select {
		case <-o.done:
			return best
		case <-ctx.Done():
			return best
		case <-time.After(poll):
		}

		active, err := o.transferring(ctx)
		if err != nil || !active {
			continue
		}
		sample := o.sample(ctx)
		if sample.Path == "" {
			if best.Path == "" {
				best.Reason = sample.Reason
			}
			continue
		}
		best = sample
		if strings.HasPrefix(sample.Path, NetworkPathDirect) {
			return best
		}
	}
}

// sample reads both machines' tailnet status once and reconciles them.
func (o *networkPathObserver) sample(ctx context.Context) networkPathSample {
	localRaw, err := o.local(ctx)
	if err != nil {
		return networkPathSample{Reason: "reading the local tailnet status failed: " + err.Error()}
	}
	localPath, err := parseTailscalePeerPath(localRaw, o.peerHost)
	if err != nil {
		return networkPathSample{Reason: "reading the local tailnet status failed: " + err.Error()}
	}
	remoteRaw, err := o.remote(ctx)
	if err != nil {
		return networkPathSample{Reason: "reading the peer's tailnet status failed: " + err.Error()}
	}
	remotePath, err := parseTailscalePeerPath(remoteRaw, o.localHost)
	if err != nil {
		return networkPathSample{Reason: "reading the peer's tailnet status failed: " + err.Error()}
	}
	return reconcilePaths(localPath, remotePath)
}

// reconcilePaths turns the two machines' views into one answer. Both sides
// have to see a direct endpoint before a run calls the path direct.
func reconcilePaths(local, remote peerPath) networkPathSample {
	switch {
	case local.CurAddr != "" && remote.CurAddr != "":
		return networkPathSample{Path: NetworkPathDirect}
	case local.CurAddr != "" || remote.CurAddr != "":
		return networkPathSample{Reason: fmt.Sprintf(
			"the two machines disagree: CurAddr is %q here and %q on the peer, so neither direct nor relay is proven",
			local.CurAddr, remote.CurAddr)}
	case local.Relay != "":
		return networkPathSample{Path: NetworkPathRelay + ":" + local.Relay}
	case remote.Relay != "":
		return networkPathSample{Path: NetworkPathRelay + ":" + remote.Relay}
	default:
		return networkPathSample{Reason: "neither machine reports a direct endpoint or a relay for the other"}
	}
}

// stop ends the sampling and returns what it saw.
func (o *networkPathObserver) stop() networkPathSample {
	close(o.done)
	return <-o.result
}
