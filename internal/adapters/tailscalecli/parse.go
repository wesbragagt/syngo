package tailscalecli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// statusJSON is the part of `tailscale status --json` this adapter reads.
// Every other field is ignored on purpose: the document grows between
// Tailscale releases and an unknown field is not an error here.
type statusJSON struct {
	Version      string              `json:"Version"`
	BackendState string              `json:"BackendState"`
	Self         *nodeJSON           `json:"Self"`
	Peer         map[string]nodeJSON `json:"Peer"`
	// MagicDNSSuffix is where older clients put the suffix.
	MagicDNSSuffix string `json:"MagicDNSSuffix"`
	// CurrentTailnet is where clients since 1.24 put it.
	CurrentTailnet *struct {
		MagicDNSSuffix string `json:"MagicDNSSuffix"`
	} `json:"CurrentTailnet"`
}

// nodeJSON is one entry of Self or Peer.
type nodeJSON struct {
	ID           string   `json:"ID"`
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	OS           string   `json:"OS"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	// Online is absent on clients older than 1.16, which only say whether
	// a node is Active.
	Online *bool `json:"Online"`
	Active bool  `json:"Active"`
	// CurAddr is the direct endpoint traffic is using right now, empty
	// when there is none.
	CurAddr string `json:"CurAddr"`
	// Relay is the DERP region traffic falls back to, empty when the
	// client names none.
	Relay string `json:"Relay"`
}

// ParsePaths reads the same document and reports how traffic reaches each
// other node, sorted by host name. A node is direct when the client names a
// current endpoint for it, relay when it names a DERP region instead, and
// unknown otherwise, with the reason spelled out. Nothing is ever inferred
// from a field the client did not set.
func ParsePaths(raw []byte) ([]ports.TailnetPath, error) {
	var doc statusJSON
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: reading tailscale status: %v", ports.ErrTailnetUnavailable, err)
	}

	paths := make([]ports.TailnetPath, 0, len(doc.Peer))
	for _, peer := range doc.Peer {
		paths = append(paths, pathOf(peer))
	}
	sort.Slice(paths, func(i, j int) bool {
		if paths[i].Node.HostName != paths[j].Node.HostName {
			return paths[i].Node.HostName < paths[j].Node.HostName
		}
		return paths[i].Node.ID < paths[j].Node.ID
	})
	return paths, nil
}

// pathOf classifies one node.
func pathOf(n nodeJSON) ports.TailnetPath {
	path := ports.TailnetPath{Node: nodeOf(n), Active: n.Active}
	switch {
	case n.CurAddr != "":
		path.Kind = ports.TailnetPathDirect
		path.Endpoint = n.CurAddr
	case n.Relay != "":
		path.Kind = ports.TailnetPathRelay
		path.Relay = n.Relay
	default:
		path.Kind = ports.TailnetPathUnknown
		path.Reason = "the local Tailscale client reports neither a direct endpoint nor a relay for this node"
	}
	return path
}

// Parse reads the document `tailscale status --json` prints. It returns this
// device's status and the other nodes of the tailnet, sorted by host name.
// Only a document that is not JSON, or that carries no Self, is an error:
// everything else is read as far as it can be.
func Parse(raw []byte) (ports.TailnetStatus, []ports.TailnetNode, error) {
	var doc statusJSON
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ports.TailnetStatus{}, nil, fmt.Errorf("%w: reading tailscale status: %v",
			ports.ErrTailnetUnavailable, err)
	}
	if doc.Self == nil {
		return ports.TailnetStatus{}, nil, fmt.Errorf("%w: tailscale status has no Self",
			ports.ErrTailnetUnavailable)
	}

	suffix := doc.MagicDNSSuffix
	if doc.CurrentTailnet != nil && doc.CurrentTailnet.MagicDNSSuffix != "" {
		suffix = doc.CurrentTailnet.MagicDNSSuffix
	}
	status := ports.TailnetStatus{
		Version:        doc.Version,
		BackendState:   doc.BackendState,
		Running:        doc.BackendState == "Running",
		MagicDNSSuffix: suffix,
		Self:           nodeOf(*doc.Self),
	}

	peers := make([]ports.TailnetNode, 0, len(doc.Peer))
	for _, peer := range doc.Peer {
		peers = append(peers, nodeOf(peer))
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].HostName != peers[j].HostName {
			return peers[i].HostName < peers[j].HostName
		}
		return peers[i].ID < peers[j].ID
	})
	return status, peers, nil
}

// nodeOf converts one JSON node into the port's shape: the trailing dot of a
// MagicDNS name is dropped, addresses are sorted IPv4 first, and a client too
// old to report Online falls back to what it says about Active.
func nodeOf(n nodeJSON) ports.TailnetNode {
	online := n.Active
	if n.Online != nil {
		online = *n.Online
	}
	return ports.TailnetNode{
		ID:       n.ID,
		HostName: n.HostName,
		DNSName:  strings.TrimSuffix(n.DNSName, "."),
		IPs:      sortIPs(n.TailscaleIPs),
		Online:   online,
		OS:       n.OS,
	}
}

// sortIPs returns the addresses with the IPv4 ones first, keeping the order
// Tailscale gave them within each family.
func sortIPs(ips []string) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if isIPv4(ip) {
			out = append(out, ip)
		}
	}
	for _, ip := range ips {
		if !isIPv4(ip) {
			out = append(out, ip)
		}
	}
	return out
}

// isIPv4 reports whether an address is written in dotted form. Tailscale
// prints plain addresses, so the shape is enough to tell the families apart.
func isIPv4(ip string) bool { return strings.Contains(ip, ".") && !strings.Contains(ip, ":") }
