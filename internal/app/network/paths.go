package network

import (
	"context"
	"strings"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
)

// PeerPaths reports how traffic reaches each of the given devices: directly,
// through a DERP relay, or unknown with the reason. Every device gets an
// entry. The answer is only ever what the local Tailscale client said: a
// device it does not list, or lists without an endpoint and without a relay,
// is unknown, and a device reached over an address that is not on the tailnet
// at all is unknown too.
func (s *Service) PeerPaths(ctx context.Context, devices []device.Device) (map[device.ID]ports.TailnetPath, error) {
	out := make(map[device.ID]ports.TailnetPath, len(devices))
	paths, err := s.tailnetPaths(ctx)
	if err != nil {
		for _, d := range devices {
			out[d.ID] = ports.TailnetPath{
				Kind:   ports.TailnetPathUnknown,
				Reason: "the local Tailscale client could not be asked: " + err.Error(),
			}
		}
		return out, err
	}

	for _, d := range devices {
		host := hostOf(d.Address)
		match, ok := matchPath(paths, host)
		if !ok {
			out[d.ID] = ports.TailnetPath{
				Kind:   ports.TailnetPathUnknown,
				Reason: "the local Tailscale client lists no tailnet node for " + quote(host),
			}
			continue
		}
		out[d.ID] = match
	}
	return out, nil
}

// tailnetPaths asks the client, or reports that there is none.
func (s *Service) tailnetPaths(ctx context.Context) ([]ports.TailnetPath, error) {
	if s.tailnet == nil {
		return nil, ports.ErrTailnetUnavailable
	}
	return s.tailnet.Paths(ctx)
}

// matchPath finds the path to the node reached at host, which is either one of
// its tailnet addresses or one of its names.
func matchPath(paths []ports.TailnetPath, host string) (ports.TailnetPath, bool) {
	if host == "" {
		return ports.TailnetPath{}, false
	}
	for _, path := range paths {
		if nodeAnswersTo(path.Node, host) {
			return path, true
		}
	}
	return ports.TailnetPath{}, false
}

// nodeAnswersTo reports whether a tailnet node is reached at host: one of its
// addresses, its host name, its MagicDNS name, or the first label of it.
func nodeAnswersTo(node ports.TailnetNode, host string) bool {
	for _, ip := range node.IPs {
		if strings.EqualFold(ip, host) {
			return true
		}
	}
	if node.HostName != "" && strings.EqualFold(node.HostName, host) {
		return true
	}
	dns := strings.TrimSuffix(node.DNSName, ".")
	if dns == "" {
		return false
	}
	return strings.EqualFold(dns, host) ||
		strings.EqualFold(strings.SplitN(dns, ".", 2)[0], host)
}

// hostOf returns the host part of a "host:port" address, with the brackets of
// an IPv6 literal removed. An address without a port is returned as it is.
// The application layer has no net package, and this is all it needs.
func hostOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, "[") {
		if end := strings.Index(addr, "]"); end > 0 {
			return addr[1:end]
		}
		return strings.Trim(addr, "[]")
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 && !strings.Contains(addr[:i], ":") {
		return addr[:i]
	}
	return addr
}

// quote wraps a value in quotes for a message, or names it as empty.
func quote(s string) string {
	if s == "" {
		return "an empty address"
	}
	return `"` + s + `"`
}
