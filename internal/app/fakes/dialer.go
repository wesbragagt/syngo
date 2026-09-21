package fakes

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// PeerScript is what a fake dialer answers for one address.
type PeerScript struct {
	Probe ports.ProbeResult
	// Err, when set, is returned instead of the probe result.
	Err error
}

// Dialer is a scripted ports.Dialer: it answers from a table of addresses
// instead of touching the network.
type Dialer struct {
	mu      sync.Mutex
	peers   map[string]PeerScript
	dialed  []string
	probed  []string
	Latency time.Duration
}

// NewDialer returns an empty dialer. Every unknown address is unreachable.
func NewDialer() *Dialer {
	return &Dialer{peers: make(map[string]PeerScript), Latency: time.Millisecond}
}

// Set installs the answer for one address.
func (d *Dialer) Set(addr string, script PeerScript) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peers[addr] = script
}

// Probed returns the addresses Probe was called with.
func (d *Dialer) Probed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.probed...)
}

// Dialed returns the addresses Dial was called with.
func (d *Dialer) Dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

// Probe returns the scripted result for addr.
func (d *Dialer) Probe(_ context.Context, addr string) (ports.ProbeResult, error) {
	d.mu.Lock()
	script, ok := d.peers[addr]
	d.probed = append(d.probed, addr)
	d.mu.Unlock()

	switch {
	case !ok:
		return ports.ProbeResult{}, &ports.ConnectError{Stage: ports.StageDial, Err: errors.New("connection refused")}
	case script.Err != nil:
		return ports.ProbeResult{}, script.Err
	}
	return script.Probe, nil
}

// Dial returns a connection to the scripted peer at addr.
func (d *Dialer) Dial(_ context.Context, addr string, expect device.ID) (ports.PeerConn, error) {
	d.mu.Lock()
	script, ok := d.peers[addr]
	d.dialed = append(d.dialed, addr)
	latency := d.Latency
	d.mu.Unlock()

	switch {
	case !ok:
		return nil, &ports.ConnectError{Stage: ports.StageDial, Err: errors.New("connection refused")}
	case script.Err != nil:
		return nil, script.Err
	case expect != "" && script.Probe.DeviceID != expect:
		return nil, &ports.ConnectError{Stage: ports.StageTLS, Err: ports.ErrDeviceMismatch}
	case !script.Probe.Accepted:
		return nil, &ports.ConnectError{
			Stage: ports.StageHandshake,
			Err:   &ports.RejectedError{Reason: script.Probe.RejectReason},
		}
	}
	return &PeerConn{
		ID:      script.Probe.DeviceID,
		Hello:   ports.HelloInfo{DeviceID: script.Probe.DeviceID, Name: script.Probe.Name},
		Latency: latency,
		changes: make(chan folder.ID, 1),
	}, nil
}

// PeerConn is an established fake connection.
type PeerConn struct {
	ID      device.ID
	Hello   ports.HelloInfo
	Latency time.Duration

	mu     sync.Mutex
	pings  int
	closed bool

	changes chan folder.ID
}

// RemoteID returns the peer's device ID.
func (c *PeerConn) RemoteID() device.ID { return c.ID }

// RemoteHello returns what the peer announced.
func (c *PeerConn) RemoteHello() ports.HelloInfo { return c.Hello }

// Ping returns the configured latency and counts the call.
func (c *PeerConn) Ping(context.Context) (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, errors.New("connection closed")
	}
	c.pings++
	return c.Latency, nil
}

// Pings returns how often Ping was called.
func (c *PeerConn) Pings() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pings
}

// RequestManifest is not scripted in this milestone.
func (c *PeerConn) RequestManifest(context.Context, folder.ID) (manifest.Manifest, error) {
	return manifest.Manifest{}, errors.New("not implemented")
}

// RequestChunk is not scripted in this milestone.
func (c *PeerConn) RequestChunk(context.Context, ports.ChunkRequest) (ports.ChunkData, error) {
	return ports.ChunkData{}, errors.New("not implemented")
}

// Notifications reports folders the peer says have changed.
func (c *PeerConn) Notifications() <-chan folder.ID { return c.changes }

// Close marks the connection closed.
func (c *PeerConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
