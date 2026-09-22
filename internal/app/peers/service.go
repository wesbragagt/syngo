// Package peers adds, lists, removes and tests the devices this one
// synchronizes with.
package peers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// ErrNotConfirmed reports that the caller did not confirm the identity of the
// device it was about to trust.
var ErrNotConfirmed = errors.New("device identity was not confirmed")

// ErrInvalidAddress reports a peer address the service cannot make sense of.
// It is a mistake in what the user typed, not a connection failure.
var ErrInvalidAddress = errors.New("invalid peer address")

// ErrSelf reports that the address answered with this device's own identity.
// A device cannot be its own peer.
var ErrSelf = errors.New("that address is this device")

// DefaultPort is the port used when an address names only a host.
const DefaultPort = 22000

// Service manages the peer table.
type Service struct {
	store       ports.DeviceStore
	dialer      ports.Dialer
	clock       ports.Clock
	identity    ports.Identity
	defaultPort int
	// tailnet turns a host name into tailnet addresses. It is nil on a
	// device with no Tailscale support wired in, and every failure it
	// reports leaves the host as the user typed it.
	tailnet ports.TailnetDiscovery
}

// SetTailnet installs the tailnet lookup used to resolve bare host names
// before the system resolver is asked. Passing nil turns the lookup off.
func (s *Service) SetTailnet(t ports.TailnetDiscovery) { s.tailnet = t }

// New returns a peer service. identity is this device, which may not be added
// as a peer of itself. defaultPort completes addresses given without one; 0
// means DefaultPort.
func New(store ports.DeviceStore, dialer ports.Dialer, clock ports.Clock,
	identity ports.Identity, defaultPort int) *Service {
	if defaultPort == 0 {
		defaultPort = DefaultPort
	}
	return &Service{store: store, dialer: dialer, clock: clock, identity: identity, defaultPort: defaultPort}
}

// AddOptions describes the peer to add.
type AddOptions struct {
	// Address is HOST or HOST:PORT.
	Address string
	// ExpectID pins the identity: a device answering with another ID is
	// refused. When it is empty the identity is learned from the peer and
	// must be confirmed.
	ExpectID device.ID
	// Name overrides the name the peer announces.
	Name string
	// Verify asks for confirmation even when ExpectID pins the identity.
	Verify bool
	// Confirm is asked to approve an identity. It reports false, or an
	// error carrying the caller's own reason, when the user declines.
	Confirm func(id device.ID, name string) (bool, error)
}

// AddResult is the stored peer and what the probe learned about it.
type AddResult struct {
	Device device.Device
	// Accepted reports whether the peer already knows us. It is false
	// until the peer runs `peer add` against this device as well.
	Accepted bool
	// ResolvedFrom is the host name the tailnet turned into the address
	// that was stored. It is empty when nothing was resolved.
	ResolvedFrom string
}

// Add probes the address, checks the identity and stores the peer. The peer
// does not have to accept us yet: pairing is complete once both sides have
// added the other.
func (s *Service) Add(ctx context.Context, o AddOptions) (AddResult, error) {
	addr, err := s.address(o.Address)
	if err != nil {
		return AddResult{}, err
	}
	addr, resolvedFrom := s.resolve(ctx, addr)

	probe, err := s.dialer.Probe(ctx, addr)
	if err != nil {
		return AddResult{}, fmt.Errorf("contacting %s: %w", addr, err)
	}
	if s.identity != nil && probe.DeviceID == s.identity.DeviceID() {
		return AddResult{}, fmt.Errorf("%w: %s answers as %s",
			ErrSelf, addr, probe.DeviceID.Short())
	}
	if o.ExpectID != "" && probe.DeviceID != o.ExpectID {
		return AddResult{}, fmt.Errorf("%w: %s answers as %s, want %s",
			ports.ErrDeviceMismatch, addr, probe.DeviceID, o.ExpectID)
	}
	if o.ExpectID == "" || o.Verify {
		if err := confirm(o.Confirm, probe.DeviceID, probe.Name); err != nil {
			return AddResult{}, err
		}
	}

	if _, err := s.store.GetDevice(ctx, probe.DeviceID); err == nil {
		return AddResult{}, fmt.Errorf("peer %s: %w", probe.DeviceID.Short(), ports.ErrAlreadyExists)
	} else if !errors.Is(err, ports.ErrNotFound) {
		return AddResult{}, err
	}

	name := o.Name
	if name == "" {
		name = probe.Name
	}
	// The identity was either pinned by the caller or confirmed by the
	// user, so the peer is stored as verified either way.
	peer := device.Device{
		ID:       probe.DeviceID,
		Name:     name,
		Address:  addr,
		Verified: true,
		AddedAt:  s.clock.Now(),
	}
	if err := s.store.SaveDevice(ctx, peer); err != nil {
		return AddResult{}, err
	}
	return AddResult{Device: peer, Accepted: probe.Accepted, ResolvedFrom: resolvedFrom}, nil
}

// List returns every known peer.
func (s *Service) List(ctx context.Context) ([]device.Device, error) {
	return s.store.ListDevices(ctx)
}

// Remove forgets a peer. The id may be given in the dashed display form.
func (s *Service) Remove(ctx context.Context, id device.ID) error {
	parsed, err := device.ParseID(string(id))
	if err != nil {
		return fmt.Errorf("%w: %s", ports.ErrNotFound, err)
	}
	return s.store.DeleteDevice(ctx, parsed)
}

// TestResult is what one connection attempt learned.
type TestResult struct {
	Address  string
	DeviceID device.ID
	Name     string
	// TLSOK reports that the TLS handshake and the Hello exchange
	// succeeded, so the identity below is trustworthy.
	TLSOK bool
	// Authenticated reports that the peer accepted us, which it only does
	// once it has added this device.
	Authenticated bool
	RejectReason  string
	// Reason is the short failure token: "dial", "tls", "timeout",
	// "device_mismatch" or "rejected:<reason>". It is empty on success.
	Reason  string
	Latency time.Duration
	Err     string
	// ResolvedFrom is the host name the tailnet turned into Address. It is
	// empty when nothing was resolved.
	ResolvedFrom string
}

// Test connects to a peer, which may be named by device ID or by address, and
// reports how far the attempt got. Connection failures are part of the result,
// not errors; only an unusable target or a broken store is an error.
func (s *Service) Test(ctx context.Context, target string) (TestResult, error) {
	result := TestResult{}
	if id, err := device.ParseID(target); err == nil {
		known, err := s.store.GetDevice(ctx, id)
		if err != nil {
			return TestResult{}, err
		}
		result.Address = known.Address
		result.DeviceID = known.ID
		result.Name = known.Name
	} else {
		addr, err := s.address(target)
		if err != nil {
			return TestResult{}, err
		}
		result.Address, result.ResolvedFrom = s.resolve(ctx, addr)
	}

	probe, probeErr := s.dialer.Probe(ctx, result.Address)
	if probeErr != nil {
		result.Reason = reasonOf(probeErr)
		result.Err = messageOf(probeErr)
		return result, nil
	}

	result.TLSOK = true
	// A device cannot test itself: what answered is this process, and
	// reporting it as a peer that refuses us would name the wrong problem.
	if s.identity != nil && probe.DeviceID == s.identity.DeviceID() {
		return TestResult{}, fmt.Errorf("%w: %s answers as %s",
			ErrSelf, result.Address, probe.DeviceID.Short())
	}
	if result.DeviceID != "" && probe.DeviceID != result.DeviceID {
		result.Err = fmt.Sprintf("%s answers as %s, want %s", result.Address, probe.DeviceID, result.DeviceID)
		result.Reason = "device_mismatch"
		return result, nil
	}
	result.DeviceID = probe.DeviceID
	if probe.Name != "" {
		result.Name = probe.Name
	}
	result.Authenticated = probe.Accepted
	result.Latency = probe.Latency
	if !probe.Accepted {
		result.RejectReason = probe.RejectReason
		result.Reason = "rejected:" + probe.RejectReason
		result.Err = "peer rejected the connection: " + probe.RejectReason
		return result, nil
	}
	if err := s.store.TouchDevice(ctx, probe.DeviceID, s.clock.Now()); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return result, err
	}
	return result, nil
}

// resolve replaces the host of a HOST:PORT address with a tailnet address
// when the host is a name the tailnet knows. The second result is the host
// name that was resolved, or empty when the address was left as it was.
//
// The resolved address is what gets stored, because tailnet addresses are
// stable and storing the name would make every dial depend on the system
// resolver knowing MagicDNS. Any failure, including no Tailscale at all,
// leaves the host as typed so the dialer can try system DNS.
func (s *Service) resolve(ctx context.Context, addr string) (string, string) {
	if s.tailnet == nil {
		return addr, ""
	}
	host, port, ok := splitHostPort(addr)
	if !ok || host == "" || isIPv4Literal(host) || isIPv6Literal(host) || isLoopbackName(host) {
		return addr, ""
	}
	ips, err := s.tailnet.Resolve(ctx, host)
	if err != nil || len(ips) == 0 {
		return addr, ""
	}
	return joinHostPort(ips[0], port), host
}

// isLoopbackName reports whether host names this machine's loopback. Such a
// name is reserved for the local host, so it is never looked up in the tailnet
// however a node there chose to name itself: a peer called "localhost" must
// not be able to take an address that means "here" away from the resolver.
func isLoopbackName(host string) bool {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	return lower == "localhost" || strings.HasSuffix(lower, ".localhost")
}

// splitHostPort splits an address this service has already completed, so it is
// either HOST:PORT or [IPv6]:PORT.
func splitHostPort(addr string) (host, port string, ok bool) {
	if strings.HasPrefix(addr, "[") {
		end := strings.LastIndex(addr, "]")
		if end < 0 || !strings.HasPrefix(addr[end+1:], ":") {
			return "", "", false
		}
		return addr[1:end], addr[end+2:], true
	}
	colon := strings.LastIndex(addr, ":")
	if colon < 0 {
		return "", "", false
	}
	return addr[:colon], addr[colon+1:], true
}

// joinHostPort writes an address, bracketing an IPv6 host.
func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// address completes a HOST or HOST:PORT target with the default port. It does
// not resolve anything: that is the transport's job.
func (s *Service) address(target string) (string, error) {
	host := strings.TrimSpace(target)
	port := ":" + strconv.Itoa(s.defaultPort)
	switch {
	case host == "":
		return "", fmt.Errorf("%w: address is empty", ErrInvalidAddress)
	case strings.HasPrefix(host, "["):
		// A bracketed IPv6 address, with or without a port.
		end := strings.LastIndex(host, "]")
		if end < 0 {
			return "", fmt.Errorf("%w: %q has no closing bracket", ErrInvalidAddress, target)
		}
		if !isIPv6Literal(host[1:end]) {
			return "", fmt.Errorf("%w: %q is not an IPv6 address", ErrInvalidAddress, target)
		}
		if end == len(host)-1 {
			return host + port, nil
		}
		if !strings.HasPrefix(host[end+1:], ":") {
			return "", fmt.Errorf("%w: %q must be HOST or HOST:PORT", ErrInvalidAddress, target)
		}
		return host, nil
	case strings.Count(host, ":") > 1:
		// A bare IPv6 address needs brackets before a port.
		if !isIPv6Literal(host) {
			return "", fmt.Errorf("%w: %q is not an IPv6 address", ErrInvalidAddress, target)
		}
		return "[" + host + "]" + port, nil
	case strings.Contains(host, ":"):
		if strings.HasSuffix(host, ":") {
			return "", fmt.Errorf("%w: %q has no port", ErrInvalidAddress, target)
		}
		return host, nil
	}
	return host + port, nil
}

// isIPv6Literal reports whether host is an IPv6 address written out: groups of
// up to four hexadecimal digits separated by colons, with at most one "::" and
// an optional dotted IPv4 tail. It is a syntax check the caller makes before
// handing the address to the transport, so a typo is reported as a mistake in
// what the user typed rather than as a peer that could not be reached.
func isIPv6Literal(host string) bool {
	if host == "" || strings.Count(host, "::") > 1 {
		return false
	}
	compressed := strings.Contains(host, "::")
	// A leading or trailing "::" stands for the groups it replaces, so the
	// empty group it leaves at that end is not a missing one.
	trimmed := strings.TrimSuffix(strings.TrimPrefix(host, "::"), "::")
	if trimmed == "" {
		return compressed
	}

	groups := strings.Split(strings.Replace(trimmed, "::", ":", 1), ":")
	if len(groups) > 8 || (!compressed && len(groups) != 8) {
		return false
	}
	for i, group := range groups {
		// The last group may be a dotted IPv4 address, as in "::ffff:10.0.0.1".
		if i == len(groups)-1 && strings.Contains(group, ".") {
			if !isIPv4Literal(group) {
				return false
			}
			continue
		}
		if len(group) == 0 || len(group) > 4 {
			return false
		}
		for _, r := range group {
			if !isHexDigit(r) {
				return false
			}
		}
	}
	return true
}

// isIPv4Literal reports whether s is four decimal octets separated by dots.
func isIPv4Literal(s string) bool {
	octets := strings.Split(s, ".")
	if len(octets) != 4 {
		return false
	}
	for _, octet := range octets {
		n, err := strconv.Atoi(octet)
		if err != nil || n < 0 || n > 255 || len(octet) > 3 {
			return false
		}
	}
	return true
}

// isHexDigit reports whether r is a hexadecimal digit.
func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// confirm asks the caller to approve an identity.
func confirm(ask func(device.ID, string) (bool, error), id device.ID, name string) error {
	if ask == nil {
		return fmt.Errorf("%w: %s", ErrNotConfirmed, id.Display())
	}
	ok, err := ask(id, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotConfirmed, id.Display())
	}
	return nil
}

// reasonOf turns a connection failure into the short token reported to users.
// messageOf is the failure without the stage a ConnectError names: the reason
// is reported beside it, and callers print the two together, so keeping the
// prefix would print it twice.
func messageOf(err error) string {
	var connErr *ports.ConnectError
	if errors.As(err, &connErr) && connErr.Err != nil {
		return connErr.Err.Error()
	}
	return err.Error()
}

func reasonOf(err error) string {
	var rejected *ports.RejectedError
	if errors.As(err, &rejected) {
		return "rejected:" + rejected.Reason
	}
	var connErr *ports.ConnectError
	if errors.As(err, &connErr) {
		return connErr.Stage
	}
	return "error"
}
