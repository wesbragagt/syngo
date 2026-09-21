package tcpwire

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

var _ ports.Dialer = (*Dialer)(nil)

// errNotImplemented is returned by the request methods whose frames the sync
// engine introduces.
var errNotImplemented = errors.New("this protocol request is not implemented yet")

// notificationBuffer is how many folder change notifications a connection
// queues before it drops them; the puller rescans anyway.
const notificationBuffer = 16

// Dialer opens authenticated connections to peers.
type Dialer struct {
	cert  tls.Certificate
	hello func() ports.HelloInfo
	log   *slog.Logger

	pingInterval time.Duration
	idleTimeout  time.Duration
	dialTimeout  time.Duration
}

// NewDialer returns a dialer presenting cert. hello supplies the Hello sent on
// every connection; it is a function because the folder list changes while the
// daemon runs.
func NewDialer(cert tls.Certificate, hello func() ports.HelloInfo, log *slog.Logger) *Dialer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Dialer{
		cert:         cert,
		hello:        hello,
		log:          log,
		pingInterval: DefaultPingInterval,
		idleTimeout:  DefaultIdleTimeout,
		dialTimeout:  DefaultDialTimeout,
	}
}

// Dial connects to addr and completes the handshake. When expect is not empty
// the peer must present exactly that identity. The returned connection is
// authenticated: the peer has answered a Ping, which it only does after it has
// accepted our Hello.
func (d *Dialer) Dial(ctx context.Context, addr string, expect device.ID) (ports.PeerConn, error) {
	c, err := d.connect(ctx, addr, expect)
	if err != nil {
		return nil, err
	}
	if _, err := c.Ping(ctx); err != nil {
		_ = c.Close()
		return nil, &ports.ConnectError{Stage: ports.StageHandshake, Err: err}
	}
	c.keepalive()
	return c, nil
}

// Probe connects only far enough to learn who answers at addr. A peer that has
// not added us yet still tells us its identity in its Hello and then rejects
// the connection; that is how pairing starts.
func (d *Dialer) Probe(ctx context.Context, addr string) (ports.ProbeResult, error) {
	c, err := d.connect(ctx, addr, "")
	if err != nil {
		return ports.ProbeResult{}, err
	}
	defer func() { _ = c.Close() }()

	result := ports.ProbeResult{DeviceID: c.remote, Name: c.hello.Name}
	latency, err := c.Ping(ctx)
	var rejected *ports.RejectedError
	switch {
	case errors.As(err, &rejected):
		result.RejectReason = rejected.Reason
		return result, nil
	case err != nil:
		return result, &ports.ConnectError{Stage: ports.StageHandshake, Err: err}
	}
	result.Accepted = true
	result.Latency = latency
	return result, nil
}

// connect performs the TCP connect, the TLS handshake and the Hello exchange,
// and starts the read loop.
func (d *Dialer) connect(ctx context.Context, addr string, expect device.ID) (*conn, error) {
	ctx, cancel := context.WithTimeout(ctx, d.dialTimeout)
	defer cancel()

	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &ports.ConnectError{Stage: stageOf(ctx, ports.StageDial), Err: err}
	}

	tlsConn := tls.Client(raw, ClientTLS(d.cert, expect))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, &ports.ConnectError{Stage: stageOf(ctx, ports.StageTLS), Err: err}
	}
	remote, err := PeerIDFromConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		return nil, &ports.ConnectError{Stage: ports.StageTLS, Err: err}
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}
	hello, err := exchangeHello(tlsConn, d.hello(), remote)
	if err != nil {
		_ = tlsConn.Close()
		return nil, &ports.ConnectError{Stage: stageOf(ctx, ports.StageHandshake), Err: err}
	}
	_ = tlsConn.SetDeadline(time.Time{})

	c := &conn{
		tls:           tlsConn,
		remote:        remote,
		hello:         hello,
		log:           d.log.With("peer", remote.Short()),
		pingInterval:  d.pingInterval,
		idleTimeout:   d.idleTimeout,
		notifications: make(chan folder.ID, notificationBuffer),
		done:          make(chan struct{}),
	}
	c.active = time.Now()
	go c.readLoop()
	return c, nil
}

// stageOf reports the timeout stage when the deadline passed, and fallback
// otherwise, so a caller can tell a refused connection from a silent one.
func stageOf(ctx context.Context, fallback string) string {
	if ctx.Err() != nil {
		return ports.StageTimeout
	}
	return fallback
}

var _ ports.PeerConn = (*conn)(nil)

// conn is an established client connection. One request is in flight at a
// time; the read loop routes folder change notifications aside and hands every
// other frame to the waiting request.
type conn struct {
	tls          *tls.Conn
	remote       device.ID
	hello        ports.HelloInfo
	log          *slog.Logger
	pingInterval time.Duration
	idleTimeout  time.Duration

	// requests serializes callers so only one request is outstanding.
	requests sync.Mutex
	// writes keeps two goroutines, such as a request and the keepalive,
	// from interleaving frames.
	writes sync.Mutex

	mu      sync.Mutex
	pending chan Frame
	active  time.Time

	notifications chan folder.ID
	done          chan struct{}
	closeOnce     sync.Once
	closeErr      error
	nonce         atomic.Uint64
}

// RemoteID returns the peer's device ID, derived from its certificate.
func (c *conn) RemoteID() device.ID { return c.remote }

// RemoteHello returns what the peer announced when it connected.
func (c *conn) RemoteHello() ports.HelloInfo { return c.hello }

// Notifications reports folders the peer says have changed. The channel is
// never closed; it simply stops delivering once the connection ends.
func (c *conn) Notifications() <-chan folder.ID { return c.notifications }

// Ping measures the round trip to the peer.
func (c *conn) Ping(ctx context.Context) (time.Duration, error) {
	nonce := c.nonce.Add(1)
	frame, err := frameOf(MsgPing, nonceHeader{Nonce: nonce}, nil)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	reply, err := c.request(ctx, frame)
	if err != nil {
		return 0, err
	}
	latency := time.Since(start)
	if reply.Type != MsgPong {
		return 0, fmt.Errorf("expected pong, got %s", reply.Type)
	}
	var header nonceHeader
	if err := decodeHeader(reply, &header); err != nil {
		return 0, err
	}
	if header.Nonce != nonce {
		return 0, fmt.Errorf("pong nonce %d, want %d", header.Nonce, nonce)
	}
	return latency, nil
}

// RequestManifest is implemented by the sync engine.
func (c *conn) RequestManifest(_ context.Context, id folder.ID) (manifest.Manifest, error) {
	return manifest.Manifest{}, fmt.Errorf("manifest of %s: %w", id, errNotImplemented)
}

// RequestChunk is implemented by the sync engine.
func (c *conn) RequestChunk(_ context.Context, req ports.ChunkRequest) (ports.ChunkData, error) {
	return ports.ChunkData{}, fmt.Errorf("chunk %d of %s: %w", req.Index, req.Path, errNotImplemented)
}

// Close shuts the connection down. It is safe to call more than once.
func (c *conn) Close() error {
	c.fail(nil)
	return nil
}

// request sends one frame and waits for the reply.
func (c *conn) request(ctx context.Context, frame Frame) (Frame, error) {
	c.requests.Lock()
	defer c.requests.Unlock()

	reply := make(chan Frame, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return Frame{}, c.err()
	default:
	}
	c.pending = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.pending = nil
		c.mu.Unlock()
	}()

	if err := c.write(frame); err != nil {
		return Frame{}, err
	}
	select {
	case f := <-reply:
		return f, nil
	case <-c.done:
		return Frame{}, c.err()
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// write sends one frame, serialized against other writers.
func (c *conn) write(frame Frame) error {
	c.writes.Lock()
	defer c.writes.Unlock()
	return WriteFrame(c.tls, frame)
}

// readLoop reads frames until the peer stops talking or closes.
func (c *conn) readLoop() {
	for {
		if err := c.tls.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
			c.fail(err)
			return
		}
		frame, err := ReadFrame(c.tls)
		if err != nil {
			c.fail(err)
			return
		}

		c.mu.Lock()
		c.active = time.Now()
		c.mu.Unlock()

		switch frame.Type {
		case MsgManifestChanged:
			c.notify(frame)
		case MsgRejected:
			var header rejectedHeader
			if err := decodeHeader(frame, &header); err != nil {
				c.fail(err)
				return
			}
			c.fail(&ports.RejectedError{Reason: header.Reason})
			return
		case MsgError:
			var header errorHeader
			if err := decodeHeader(frame, &header); err != nil {
				c.fail(err)
				return
			}
			c.fail(fmt.Errorf("peer reported %s: %s", header.Code, header.Message))
			return
		default:
			c.deliver(frame)
		}
	}
}

// notify forwards a folder change to the notification channel, dropping it
// when the consumer is behind: the next sync cycle sees the change anyway.
func (c *conn) notify(frame Frame) {
	var header struct {
		FolderID string `json:"folder_id"`
	}
	if err := decodeHeader(frame, &header); err != nil {
		c.log.Warn("bad manifest change notification", "error", err)
		return
	}
	select {
	case c.notifications <- folder.ID(header.FolderID):
	default:
		c.log.Debug("dropped folder change notification", "folder", header.FolderID)
	}
}

// deliver hands a reply to the waiting request, or drops it when the request
// was abandoned.
func (c *conn) deliver(frame Frame) {
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	if pending == nil {
		c.log.Debug("unexpected frame", "type", frame.Type.String())
		return
	}
	pending <- frame
}

// keepalive pings the peer whenever the connection has been idle, so the
// peer's read deadline does not fire.
func (c *conn) keepalive() {
	go func() {
		ticker := time.NewTicker(c.pingInterval / 2)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-ticker.C:
				c.mu.Lock()
				idle := time.Since(c.active)
				c.mu.Unlock()
				if idle < c.pingInterval {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), c.idleTimeout)
				_, err := c.Ping(ctx)
				cancel()
				if err != nil {
					c.log.Debug("keepalive ping failed", "error", err)
				}
			}
		}
	}()
}

// fail closes the connection once, remembering why.
func (c *conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.done)
		_ = c.tls.Close()
	})
}

// err reports why the connection ended.
func (c *conn) err() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	if c.closeErr == nil {
		return net.ErrClosed
	}
	return c.closeErr
}
