package tcpwire

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

var _ ports.Listener = (*Listener)(nil)

// MaxConcurrentHandshakes is how many connections may be in their TLS
// handshake or Hello exchange at once. Everything before the Hello is work
// anyone who reaches the port can ask for, so the number of connections doing
// it is bounded; the rest wait their turn and time out if they have to.
const MaxConcurrentHandshakes = 32

// MaxConcurrentRequests is how many requests of one connected peer are
// answered at the same time. A client multiplexes chunk requests to keep the
// link busy while it writes what already arrived, so a server that answered
// one at a time would be the bottleneck. The number bounds what one peer can
// make this device read into memory at once: at the 16 MiB maximum chunk size
// that is 256 MiB across all of them, and a real chunk is around the
// configured average of 1 MiB.
const MaxConcurrentRequests = 16

// Listener accepts peer connections on a bound TCP port.
type Listener struct {
	cert tls.Certificate
	log  *slog.Logger
	ln   net.Listener

	handshakeTimeout time.Duration
	idleTimeout      time.Duration
	// handshakes holds one token per connection that is handshaking.
	handshakes chan struct{}

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
		handshakes:       make(chan struct{}, MaxConcurrentHandshakes),
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

// Close stops accepting connections and drops the ones already accepted.
// Serve returns shortly after: it waits for the connections it accepted, and
// a listener is closed because the address under it is gone, which leaves
// those connections dead whether or not the peers have noticed.
func (l *Listener) Close() error {
	err := l.ln.Close()

	l.mu.Lock()
	conns := make([]*serverConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.tls.Close()
	}
	return err
}

// Broadcast tells the peers authorized for a folder that it changed. A peer
// the folder was never granted to is not told, so the notification does not
// name a folder its Hello left out.
func (l *Listener) Broadcast(id folder.ID, seq uint64) {
	frame, err := frameOf(MsgManifestChanged, manifestChangedHeader{FolderID: string(id), Sequence: seq}, nil)
	if err != nil {
		l.log.Error("encode folder change notification", "folder", id, "error", err)
		return
	}
	for _, c := range l.connections() {
		served := c.advertised(context.Background())
		if !slices.ContainsFunc(served, func(a ports.FolderAdvert) bool { return a.ID == id }) {
			continue
		}
		l.write(c, frame)
	}
}

// AnnounceFolders tells every connected peer which folders this device serves
// now. It replaces the list they received in the Hello, so a folder added,
// given another mode, granted or revoked while the connection is up takes
// effect without a reconnect.
//
// adverts says what this device serves and in which mode; the handler says
// which of those folders this peer may know about at all. A peer is told the
// intersection, so an announcement never names a folder its Hello left out.
func (l *Listener) AnnounceFolders(adverts []ports.FolderAdvert) {
	for _, c := range l.connections() {
		visible := make(map[folder.ID]struct{})
		for _, a := range c.advertised(context.Background()) {
			visible[a.ID] = struct{}{}
		}
		allowed := make([]ports.FolderAdvert, 0, len(adverts))
		for _, a := range adverts {
			if _, ok := visible[a.ID]; ok {
				allowed = append(allowed, a)
			}
		}
		frame, err := frameOf(MsgFoldersChanged, foldersChangedHeader{Folders: toWireAdverts(allowed)}, nil)
		if err != nil {
			l.log.Error("encode folder announcement", "error", err)
			return
		}
		l.write(c, frame)
	}
}

// RetainDevices closes the connections of peers that are not in ids. It is
// how a device removed from the table stops being served at once, rather than
// until it hangs up by itself.
func (l *Listener) RetainDevices(ids []device.ID) {
	keep := make(map[device.ID]struct{}, len(ids))
	for _, id := range ids {
		keep[id] = struct{}{}
	}

	l.mu.Lock()
	var gone []*serverConn
	for c := range l.conns {
		if _, ok := keep[c.remote]; !ok {
			gone = append(gone, c)
		}
	}
	l.mu.Unlock()

	for _, c := range gone {
		l.log.Info("dropping a peer that is no longer known", "peer", c.remote.Short())
		_ = c.tls.Close()
	}
}

// connections returns the peers connected right now.
func (l *Listener) connections() []*serverConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	conns := make([]*serverConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	return conns
}

// write sends one unsolicited frame to one peer.
func (l *Listener) write(c *serverConn, frame Frame) {
	if err := c.write(frame); err != nil {
		l.log.Debug("notify peer", "peer", c.remote.Short(), "type", frame.Type.String(), "error", err)
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

	// The slot is held for the handshake and the Hello only, so a peer
	// that is connected does not keep one.
	select {
	case l.handshakes <- struct{}{}:
	case <-hsCtx.Done():
		l.log.Debug("no handshake slot in time", "remote", raw.RemoteAddr().String())
		return
	}
	release := sync.OnceFunc(func() { <-l.handshakes })
	defer release()

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
	if _, err := exchangeHello(conn, h.Hello(ctx, remote), remote); err != nil {
		l.log.Debug("hello exchange failed", "peer", remote.Short(), "error", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})
	release()

	// The Hello is answered before the device is checked on purpose: an
	// unpaired peer running `peer add` against us learns our identity from
	// it, which is how pairing starts.
	if known == nil || !known(ctx, remote) {
		l.log.Info("rejected unknown device", "peer", remote.Short(), "remote", raw.RemoteAddr().String())
		_ = writeRejected(conn, ReasonUnknownDevice)
		return
	}

	sc := &serverConn{tls: conn, remote: remote, log: l.log.With("peer", remote.Short()), handler: h, known: known}
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
	// known re-checks before every request that the peer is still a device
	// this one synchronizes with.
	known  ports.DeviceLookup
	writes sync.Mutex
}

// advertised is the folder list this device announces to this peer: the
// folders it is authorized for, as the handler reports them now.
func (c *serverConn) advertised(ctx context.Context) []ports.FolderAdvert {
	return c.handler.Hello(ctx, c.remote).Folders
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
//
// Requests are answered concurrently, at most MaxConcurrentRequests of one
// peer at a time, because a client may have several in flight. Each reply
// carries the request id of the frame it answers, so the client can tell them
// apart whatever order they come back in. The read loop itself does no
// application work: a request that waits on the disk or on the upload limiter
// must not hold up the one behind it.
func (c *serverConn) serve(ctx context.Context, idleTimeout time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var answering sync.WaitGroup
	defer answering.Wait()
	// The slots bound how much of this device a single peer can occupy,
	// and with it how many chunks it can have read into memory at once.
	slots := make(chan struct{}, MaxConcurrentRequests)

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

		switch frame.Type {
		case MsgFoldersChanged:
			// The serving side answers requests about its own
			// folders and keeps no view of the peer's, so the
			// announcement is accepted and dropped rather than
			// ending a connection the peer considers healthy.
			c.log.Debug("peer announced its folders")
			continue
		case MsgPing, MsgManifestRequest, MsgChunkListRequest, MsgChunkRequest:
		default:
			c.refuse(frame, CodeBadRequest, fmt.Sprintf("%s frames are not accepted", frame.Type))
			return
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		answering.Add(1)
		go func(frame Frame) {
			defer answering.Done()
			defer func() { <-slots }()
			c.answer(ctx, frame)
		}(frame)
	}
}

// answer serves one request and writes its reply. A failure this side cannot
// classify ends the connection, which unblocks the read loop.
func (c *serverConn) answer(ctx context.Context, frame Frame) {
	// The device table may have changed since the handshake: a peer removed
	// while it was connected is told so and its connection ends here. The
	// check is per request, alongside the folder authorization the handler
	// makes, so neither is skipped by a peer that pipelines its requests.
	if c.known != nil && !c.known(ctx, c.remote) {
		c.log.Info("peer is no longer a known device")
		c.refuse(frame, CodeUnknownDevice, "this device is no longer a known peer")
		return
	}

	var (
		reply Frame
		err   error
	)
	switch frame.Type {
	case MsgPing:
		reply, err = c.pong(frame)
	case MsgManifestRequest:
		reply, err = c.manifest(ctx, frame)
	case MsgChunkListRequest:
		reply, err = c.chunkList(ctx, frame)
	default:
		reply, err = c.chunk(ctx, frame)
	}
	if err != nil {
		// A header this side cannot read means the peers no longer
		// agree on the protocol.
		c.refuse(frame, CodeBadRequest, err.Error())
		return
	}
	if err := c.write(reply.withID(frame.ReqID)); err != nil {
		_ = c.tls.Close()
	}
}

// refuse answers a request with an Error frame and ends the connection. The
// close is what stops the read loop, which is blocked on the peer.
func (c *serverConn) refuse(frame Frame, code, message string) {
	if reply, err := frameOf(MsgError, errorHeader{Code: code, Message: message}, nil); err == nil {
		_ = c.write(reply.withID(frame.ReqID))
	}
	_ = c.tls.Close()
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

// chunkList answers a chunk list request, or reports why it cannot.
func (c *serverConn) chunkList(ctx context.Context, frame Frame) (Frame, error) {
	var header chunkListRequestHeader
	if err := decodeHeader(frame, &header); err != nil {
		return Frame{}, err
	}
	hash, err := manifest.ParseHash(header.Hash)
	if err != nil {
		return Frame{}, fmt.Errorf("chunk list request for %q: %w", header.Path, err)
	}

	chunks, err := c.handler.Chunks(ctx, c.remote, ports.ChunkListRequest{
		FolderID: folder.ID(header.FolderID), Path: header.Path, Hash: hash,
	})
	var refused *ports.ChunkError
	switch {
	case errors.As(err, &refused):
		return frameOf(MsgChunkError, chunkErrorHeader{
			FolderID: string(refused.FolderID), Path: refused.Path,
			Index: refused.Index, Reason: string(refused.Reason),
		}, nil)
	case err != nil:
		c.log.Debug("chunk list refused", "folder", header.FolderID, "path", header.Path, "error", err)
		return frameOf(MsgError, errorHeader{Code: errorCode(err), Message: err.Error()}, nil)
	}
	return frameOf(MsgChunkListReply, chunkListReplyHeader{
		FolderID: header.FolderID, Path: header.Path, Count: len(chunks),
	}, encodeChunkList(chunks))
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
