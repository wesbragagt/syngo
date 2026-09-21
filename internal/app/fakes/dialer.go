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
	// Handler answers the manifest and chunk requests of connections to
	// this address. A nil handler refuses them.
	Handler ports.PeerHandler
}

// Dialer is a scripted ports.Dialer: it answers from a table of addresses
// instead of touching the network.
type Dialer struct {
	mu      sync.Mutex
	peers   map[string]PeerScript
	dialed  []string
	probed  []string
	conns   []*PeerConn
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
	conn := NewPeerConn(script.Probe.DeviceID, script.Probe.Name, script.Handler, latency)
	d.mu.Lock()
	d.conns = append(d.conns, conn)
	d.mu.Unlock()
	return conn, nil
}

// Conns returns the connections Dial handed out, in order.
func (d *Dialer) Conns() []*PeerConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*PeerConn(nil), d.conns...)
}

// NewPeerConn returns an established fake connection answering through
// handler. A nil handler refuses every manifest and chunk request.
func NewPeerConn(id device.ID, name string, handler ports.PeerHandler, latency time.Duration) *PeerConn {
	hello := ports.HelloInfo{DeviceID: id, Name: name}
	if handler != nil {
		hello = handler.Hello(context.Background())
		hello.DeviceID = id
	}
	return &PeerConn{
		ID:      id,
		Hello:   hello,
		Handler: handler,
		Latency: latency,
		changes: make(chan folder.ID, 16),
	}
}

// PeerConn is an established fake connection. It answers requests through its
// handler and records what was asked of it.
type PeerConn struct {
	ID      device.ID
	Hello   ports.HelloInfo
	Handler ports.PeerHandler
	Latency time.Duration

	mu        sync.Mutex
	pings     int
	closed    bool
	manifests []folder.ID
	chunks    []ports.ChunkRequest

	changes chan folder.ID
}

// RemoteID returns the peer's device ID.
func (c *PeerConn) RemoteID() device.ID { return c.ID }

// RemoteHello returns what the peer announced, with the folder list as the
// last SetFolders left it.
func (c *PeerConn) RemoteHello() ports.HelloInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Hello
}

// SetFolders replaces the folder list the peer announced and notifies the
// consumer, the way a FoldersChanged frame does on a real connection.
func (c *PeerConn) SetFolders(adverts []ports.FolderAdvert) {
	c.mu.Lock()
	c.Hello.Folders = adverts
	c.mu.Unlock()
	c.Notify("")
}

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

// RequestManifest returns what the handler knows about a folder.
func (c *PeerConn) RequestManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error) {
	c.mu.Lock()
	c.manifests = append(c.manifests, id)
	handler := c.Handler
	c.mu.Unlock()
	if handler == nil {
		return manifest.Manifest{}, errors.New("this fake peer serves no manifests")
	}
	return handler.Manifest(ctx, c.ID, id)
}

// RequestChunk returns one chunk from the handler.
func (c *PeerConn) RequestChunk(ctx context.Context, req ports.ChunkRequest) (ports.ChunkData, error) {
	c.mu.Lock()
	c.chunks = append(c.chunks, req)
	handler := c.Handler
	c.mu.Unlock()
	if handler == nil {
		return ports.ChunkData{}, errors.New("this fake peer serves no chunks")
	}
	return handler.Chunk(ctx, c.ID, req)
}

// ChunkRequests returns every chunk request the connection carried, in order.
func (c *PeerConn) ChunkRequests() []ports.ChunkRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.ChunkRequest(nil), c.chunks...)
}

// ManifestRequests returns the folders whose manifest was asked for.
func (c *PeerConn) ManifestRequests() []folder.ID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]folder.ID(nil), c.manifests...)
}

// Notify queues a folder change notification.
func (c *PeerConn) Notify(id folder.ID) {
	select {
	case c.changes <- id:
	default:
	}
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
