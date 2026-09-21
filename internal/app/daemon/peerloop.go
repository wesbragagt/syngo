package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// Backoff bounds for reconnecting to a peer.
const (
	peerBackoffMin = time.Second
	peerBackoffMax = 30 * time.Second
)

// DefaultPeerPingInterval is how often a connected peer is pinged, which both
// keeps the connection alive and measures the latency status reports.
const DefaultPeerPingInterval = 30 * time.Second

// peerLoop keeps one connection to one peer alive and reports its state.
type peerLoop struct {
	daemon *Daemon
	peer   device.Device
	cancel context.CancelFunc
	done   chan struct{}
}

// newPeerLoop returns a loop for a peer. Call start to run it.
func newPeerLoop(d *Daemon, peer device.Device) *peerLoop {
	return &peerLoop{daemon: d, peer: peer, done: make(chan struct{})}
}

// start runs the loop in its own goroutine.
func (l *peerLoop) start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go func() {
		defer close(l.done)
		defer cancel()
		l.run(ctx)
	}()
}

// stop cancels the loop and waits for it to return.
func (l *peerLoop) stop() {
	l.cancel()
	<-l.done
	l.daemon.setPeerState(l.peer.ID, nil)
}

// run dials the peer, keeps the connection alive and retries with backoff.
func (l *peerLoop) run(ctx context.Context) {
	log := l.daemon.deps.Log.With("peer", l.peer.ID.Short(), "address", l.peer.Address)
	backoff := peerBackoffMin

	for ctx.Err() == nil {
		conn, err := l.daemon.deps.Dialer.Dial(ctx, l.peer.Address, l.peer.ID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Debug("peer unreachable", "error", err, "retry_in", backoff)
			l.daemon.setPeerState(l.peer.ID, &status.PeerLive{Error: err.Error()})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, peerBackoffMax)
			continue
		}

		backoff = peerBackoffMin
		log.Info("peer connected")
		l.serve(ctx, conn)
		log.Info("peer disconnected")
	}
}

// serve keeps one established connection alive until it fails.
func (l *peerLoop) serve(ctx context.Context, conn ports.PeerConn) {
	defer func() { _ = conn.Close() }()
	log := l.daemon.deps.Log.With("peer", l.peer.ID.Short())

	// The first ping measures the latency status reports right away.
	if !l.ping(ctx, conn, log) {
		return
	}
	ticker := time.NewTicker(l.daemon.cfg.peerPingInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Notifications():
			// The sync engine reacts to these; until then a change
			// notification only proves the peer is alive.
		case <-ticker.C:
			if !l.ping(ctx, conn, log) {
				return
			}
		}
	}
}

// ping measures the round trip and records it. It reports whether the
// connection is still usable.
func (l *peerLoop) ping(ctx context.Context, conn ports.PeerConn, log *slog.Logger) bool {
	latency, err := conn.Ping(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Debug("peer ping failed", "error", err)
			l.daemon.setPeerState(l.peer.ID, &status.PeerLive{Error: err.Error()})
		}
		return false
	}
	l.seen(ctx, latency)
	return true
}

// seen records that the peer answered, in the report and in the database.
func (l *peerLoop) seen(ctx context.Context, latency time.Duration) {
	l.daemon.setPeerState(l.peer.ID, &status.PeerLive{Connected: true, Latency: latency})
	if err := l.daemon.deps.Store.TouchDevice(ctx, l.peer.ID, l.daemon.deps.Clock.Now()); err != nil && ctx.Err() == nil {
		l.daemon.deps.Log.Debug("recording peer contact", "peer", l.peer.ID.Short(), "error", err)
	}
}
