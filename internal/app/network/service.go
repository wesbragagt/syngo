// Package network answers what the local Tailscale client knows: whether it
// is there at all, and which devices share its tailnet.
package network

import (
	"context"
	"errors"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// Detection is the answer to "is Tailscale usable on this device". A device
// without Tailscale is described by Present being false, not by an error:
// detection asked a question and "no" is an answer.
type Detection struct {
	// Present reports that a Tailscale client answered.
	Present bool
	// Running reports that its backend is up, so the tailnet can be used.
	Running bool
	// BackendState, Version, Interface, MagicDNSSuffix, HostName and
	// DNSName are empty when Present is false.
	BackendState   string
	Version        string
	Interface      string
	MagicDNSSuffix string
	HostName       string
	DNSName        string
	// IPs are this device's tailnet addresses, IPv4 first. Never nil.
	IPs []string
}

// Service reports on the tailnet this device is part of.
type Service struct {
	tailnet ports.TailnetDiscovery
}

// New returns a network service. A nil tailnet makes Detect report an absent
// client and Peers report ports.ErrTailnetUnavailable, which is what a build
// without the adapter should say.
func New(t ports.TailnetDiscovery) *Service { return &Service{tailnet: t} }

// Detect reports whether Tailscale is installed and usable here.
func (s *Service) Detect(ctx context.Context) (Detection, error) {
	absent := Detection{IPs: []string{}}
	if s.tailnet == nil {
		return absent, nil
	}
	status, err := s.tailnet.Status(ctx)
	if err != nil {
		if errors.Is(err, ports.ErrTailnetUnavailable) {
			return absent, nil
		}
		return Detection{}, err
	}
	ips := status.Self.IPs
	if ips == nil {
		ips = []string{}
	}
	return Detection{
		Present:        true,
		Running:        status.Running,
		BackendState:   status.BackendState,
		Version:        status.Version,
		Interface:      status.Interface,
		MagicDNSSuffix: status.MagicDNSSuffix,
		HostName:       status.Self.HostName,
		DNSName:        status.Self.DNSName,
		IPs:            ips,
	}, nil
}

// Peers lists the other devices of the tailnet. Unlike Detect it passes
// ports.ErrTailnetUnavailable through: the listing that was asked for cannot
// exist without a Tailscale client.
func (s *Service) Peers(ctx context.Context) ([]ports.TailnetNode, error) {
	if s.tailnet == nil {
		return nil, ports.ErrTailnetUnavailable
	}
	return s.tailnet.Peers(ctx)
}
