package tcpwire

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

var _ ports.Listener = (*Listener)(nil)

// Listener accepts peer connections on a bound TCP port.
type Listener struct {
	cert tls.Certificate
	log  *slog.Logger
	ln   net.Listener

	handshakeTimeout time.Duration
	idleTimeout      time.Duration

	mu    sync.Mutex
	conns map[*serverConn]struct{}
}

// Listen binds addr and returns a listener for it. The address is bound here
// rather than in Serve so the caller can report the port a ":0" bind chose.
func Listen(cert tls.Certificate, addr string, log *slog.Logger) (*Listener, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return &Listener{
		cert:             cert,
		log:              log,
		ln:               ln,
		handshakeTimeout: DefaultDialTimeout,
		idleTimeout:      DefaultIdleTimeout,
		conns:            make(map[*serverConn]struct{}),
	}, nil
}

// Addr returns the bound address.
func (l *Listener) Addr() string { return l.ln.Addr().String() }

// Serve accepts connections until ctx is done. Every accepted peer runs in its
// own goroutine; Serve returns once they have all stopped.
func (l *Listener) Serve(ctx context.Context, h ports.PeerHandler, known ports.DeviceLookup) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = l.ln.Close()
	}()

	var peers sync.WaitGroup
	defer peers.Wait()

	l.log.Info("listening for peers", "address", l.Addr())
	for {
		raw, err := l.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept on %s: %w", l.Addr(), err)
		}
		peers.Add(1)
		go func() {
			defer peers.Done()
			l.handle(ctx, raw, h, known)
		}()
	}
}

// Close stops accepting connections. Serve returns shortly after.
func (l *Listener) Close() error { return l.ln.Close() }

// Broadcast tells every connected peer that a folder changed.
func (l *Listener) Broadcast(id folder.ID, seq uint64) {
	frame, err := frameOf(MsgManifestChanged, manifestChangedHeader{FolderID: string(id), Sequence: seq}, nil)
	if err != nil {
		l.log.Error("encode folder change notification", "folder", id, "error", err)
		return
	}
	l.send(frame)
}

// AnnounceFolders tells every connected peer which folders this device serves
// now. It replaces the list they received in the Hello, so a folder added or
// given another mode while the connection is up takes effect without a
// reconnect.
func (l *Listener) AnnounceFolders(adverts []ports.FolderAdvert) {
	frame, err := frameOf(MsgFoldersChanged, foldersChangedHeader{Folders: toWireAdverts(adverts)}, nil)
	if err != nil {
		l.log.Error("encode folder announcement", "error", err)
		return
	}
	l.send(frame)
}

// send writes one unsolicited frame to every connected peer.
func (l *Listener) send(frame Frame) {
	l.mu.Lock()
	conns := make([]*serverConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()

	for _, c := range conns {
		if err := c.write(frame); err != nil {
			l.log.Debug("notify peer", "peer", c.remote.Short(), "type", frame.Type.String(), "error", err)
		}
	}
}

// handle runs the handshake and then serves one peer. The connection is
// closed as soon as the listener is asked to stop, so a peer that is quiet but
// still connected does not hold the shutdown up until its read deadline.
func (l *Listener) handle(ctx context.Context, raw net.Conn, h ports.PeerHandler, known ports.DeviceLookup) {
	conn := tls.Server(raw, ServerTLS(l.cert))
	defer func() { _ = conn.Close() }()

	served, stopWatching := context.WithCancel(context.Background())
	defer stopWatching()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-served.Done():
		}
	}()

	hsCtx, cancel := context.WithTimeout(ctx, l.handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hsCtx); err != nil {
		l.log.Debug("tls handshake failed", "remote", raw.RemoteAddr().String(), "error", err)
		return
	}
	remote, err := PeerIDFromConn(conn)
	if err != nil {
		l.log.Debug("peer identity", "remote", raw.RemoteAddr().String(), "error", err)
		return
	}

	if deadline, ok := hsCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := exchangeHello(conn, h.Hello(ctx), remote); err != nil {
		l.log.Debug("hello exchange failed", "peer", remote.Short(), "error", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})

	// The Hello is answered before the device is checked on purpose: an
	// unpaired peer running `peer add` against us learns our identity from
	// it, which is how pairing starts.
	if known == nil || !known(ctx, remote) {
		l.log.Info("rejected unknown device", "peer", remote.Short(), "remote", raw.RemoteAddr().String())
		_ = writeRejected(conn, ReasonUnknownDevice)
		return
	}

	sc := &serverConn{tls: conn, remote: remote, log: l.log.With("peer", remote.Short()), handler: h}
	l.add(sc)
	defer l.remove(sc)
	l.log.Info("peer connected", "peer", remote.Short(), "remote", raw.RemoteAddr().String())
	sc.serve(ctx, l.idleTimeout)
	l.log.Info("peer disconnected", "peer", remote.Short())
}

// add registers a connection for broadcasts.
func (l *Listener) add(c *serverConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.conns[c] = struct{}{}
}

// remove forgets a connection.
func (l *Listener) remove(c *serverConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.conns, c)
}

// serverConn is one accepted peer.
type serverConn struct {
	tls     *tls.Conn
	remote  device.ID
	log     *slog.Logger
	handler ports.PeerHandler
	writes  sync.Mutex
}

// write sends one frame, serialized against broadcasts.
func (c *serverConn) write(frame Frame) error {
	c.writes.Lock()
	defer c.writes.Unlock()
	return WriteFrame(c.tls, frame)
}

// serve answers requests until the peer goes silent or closes. A request the
// application refuses is answered and the connection stays up; only a frame
// this side cannot parse ends it.
func (c *serverConn) serve(ctx context.Context, idleTimeout time.Duration) {
	for {
		if err := c.tls.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			return
		}
		frame, err := ReadFrame(c.tls)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				c.log.Debug("peer read failed", "error", err)
			}
			return
		}

		var reply Frame
		switch frame.Type {
		case MsgFoldersChanged:
			// The serving side answers requests about its own
			// folders and keeps no view of the peer's, so the
			// announcement is accepted and dropped rather than
			// ending a connection the peer considers healthy.
			c.log.Debug("peer announced its folders")
			continue
		case MsgPing:
			reply, err = c.pong(frame)
		case MsgManifestRequest:
			reply, err = c.manifest(ctx, frame)
		case MsgChunkRequest:
			reply, err = c.chunk(ctx, frame)
		default:
			_ = writeError(c.tls, CodeBadRequest, fmt.Sprintf("%s frames are not accepted", frame.Type))
			return
		}
		if err != nil {
			// A header this side cannot read means the peers no
			// longer agree on the protocol.
			_ = writeError(c.tls, CodeBadRequest, err.Error())
			return
		}
		if err := c.write(reply); err != nil {
			return
		}
	}
}

// pong answers a keepalive.
func (c *serverConn) pong(frame Frame) (Frame, error) {
	var header nonceHeader
	if err := decodeHeader(frame, &header); err != nil {
		return Frame{}, err
	}
	return frameOf(MsgPong, header, nil)
}

// manifest answers a manifest request, or reports why it cannot.
func (c *serverConn) manifest(ctx context.Context, frame Frame) (Frame, error) {
	var header manifestRequestHeader
	if err := decodeHeader(frame, &header); err != nil {
		return Frame{}, err
	}
	m, err := c.handler.Manifest(ctx, c.remote, folder.ID(header.FolderID))
	if err != nil {
		c.log.Debug("manifest refused", "folder", header.FolderID, "error", err)
		return frameOf(MsgError, errorHeader{Code: errorCode(err), Message: err.Error()}, nil)
	}
	return frameOf(MsgManifestReply, toWireManifest(m), nil)
}

// chunk answers a chunk request, or reports why it cannot.
func (c *serverConn) chunk(ctx context.Context, frame Frame) (Frame, error) {
	var header chunkRequestHeader
	if err := decodeHeader(frame, &header); err != nil {
		return Frame{}, err
	}
	req, err := fromWireChunkRequest(header)
	if err != nil {
		return Frame{}, err
	}

	data, err := c.handler.Chunk(ctx, c.remote, req)
	var refused *ports.ChunkError
	switch {
	case errors.As(err, &refused):
		return frameOf(MsgChunkError, chunkErrorHeader{
			FolderID: string(refused.FolderID), Path: refused.Path,
			Index: refused.Index, Reason: string(refused.Reason),
		}, nil)
	case err != nil:
		c.log.Debug("chunk refused", "folder", header.FolderID, "path", header.Path, "error", err)
		return frameOf(MsgError, errorHeader{Code: errorCode(err), Message: err.Error()}, nil)
	}
	return frameOf(MsgChunkData, chunkDataHeader{
		FolderID:  header.FolderID,
		Path:      header.Path,
		Index:     data.Index,
		Offset:    data.Offset,
		Length:    len(data.Data),
		ChunkHash: data.ChunkHash.String(),
	}, data.Data)
}
