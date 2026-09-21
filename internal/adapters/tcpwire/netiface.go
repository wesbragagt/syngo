package tcpwire

import (
	"errors"
	"fmt"
	"net"
)

// ErrPublicAddress reports a listen address reachable from outside the machine
// or its private network. Binding one needs an explicit opt-in.
var ErrPublicAddress = errors.New("refusing to listen on a public address")

// tailscaleRange is the CGNAT range Tailscale hands out. Addresses in it are
// treated as private: reaching them already requires being on the tailnet.
var tailscaleRange = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// ResolveListen turns the configured listen address and interface name into
// the address to bind. A non-empty iface replaces the host with the first IPv4
// address of that interface. Unless allowPublic is set, the result must be a
// loopback, private or Tailscale address; a wildcard bind counts as public.
func ResolveListen(listen, iface string, allowPublic bool) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q must be host:port: %w", listen, err)
	}

	if iface != "" {
		host, err = firstIPv4(iface)
		if err != nil {
			return "", err
		}
	}
	if allowPublic {
		return net.JoinHostPort(host, port), nil
	}

	if host == "" {
		return "", fmt.Errorf("%w: %q listens on every interface, pass --allow-public to allow it", ErrPublicAddress, listen)
	}
	ip, err := resolveHost(host)
	if err != nil {
		return "", err
	}
	if !isLocalOrPrivate(ip) {
		return "", fmt.Errorf("%w: %s, pass --allow-public to allow it", ErrPublicAddress, ip)
	}
	return net.JoinHostPort(host, port), nil
}

// firstIPv4 returns the first IPv4 address configured on an interface.
func firstIPv4(name string) (string, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("interface %q: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("addresses of interface %q: %w", name, err)
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ip := ipNet.IP.To4(); ip != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("interface %q has no IPv4 address", name)
}

// resolveHost turns a host, which may be a name, into an IP address.
func resolveHost(host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve listen host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve listen host %q: no addresses", host)
	}
	return ips[0], nil
}

// isLocalOrPrivate reports whether an address is reachable only from this
// machine, its local network or its tailnet.
func isLocalOrPrivate(ip net.IP) bool {
	if ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && tailscaleRange.Contains(v4) {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
