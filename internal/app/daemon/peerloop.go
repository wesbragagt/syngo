package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
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

// serve keeps one established connection alive and pulls from it until it
// fails. A sync cycle runs on connect, whenever the peer says one of its
// folders changed, and on every sync interval tick.
func (l *peerLoop) serve(ctx context.Context, conn ports.PeerConn) {
	defer func() { _ = conn.Close() }()
	log := l.daemon.deps.Log.With("peer", l.peer.ID.Short())

	// The first ping measures the latency status reports right away.
	if !l.ping(ctx, conn, log) {
		return
	}
	if !l.syncCheck(ctx, conn, "", log) {
		return
	}

	pings := time.NewTicker(l.daemon.cfg.peerPingInterval())
	defer pings.Stop()
	syncs := time.NewTicker(l.daemon.cfg.syncInterval())
	defer syncs.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case id := <-conn.Notifications():
			if !l.syncCheck(ctx, conn, id, log) {
				return
			}
		case <-syncs.C:
			if !l.syncCheck(ctx, conn, "", log) {
				return
			}
		case <-pings.C:
			if !l.ping(ctx, conn, log) {
				return
			}
		}
	}
}

// syncCheck runs a sync cycle and reports whether the connection is still
// usable afterwards. A cycle that failed is followed by a ping, so a peer that
// went away is noticed now instead of at the next keepalive.
func (l *peerLoop) syncCheck(ctx context.Context, conn ports.PeerConn, id folder.ID, log *slog.Logger) bool {
	if l.sync(ctx, conn, id) {
		return true
	}
	return ctx.Err() == nil && l.ping(ctx, conn, log)
}

// sync pulls one folder, or every folder both sides share when id is empty. It
// reports whether every folder synced without an error.
func (l *peerLoop) sync(ctx context.Context, conn ports.PeerConn, id folder.ID) bool {
	if l.daemon.deps.Syncer == nil {
		return true
	}
	ok := true
	for _, f := range l.syncable(ctx, conn, id) {
		if ctx.Err() != nil {
			return false
		}
		l.daemon.setState(f.ID, status.StateSyncing)
		summary, err := l.daemon.deps.Syncer.SyncFolder(ctx, conn, f)
		l.daemon.setState(f.ID, "")
		if err != nil {
			ok = false
			if ctx.Err() == nil {
				l.daemon.deps.Log.Warn("folder sync failed",
					"folder", f.ID, "peer", l.peer.ID.Short(), "error", err)
			}
			continue
		}
		if summary.Pulled > 0 || summary.Deleted > 0 || summary.Dirs > 0 || summary.Symlinks > 0 {
			l.daemon.deps.Log.Info("folder synced", "folder", f.ID, "peer", l.peer.ID.Short(),
				"pulled", summary.Pulled, "deleted", summary.Deleted, "bytes", summary.Bytes)
		}
	}
	return ok
}

// syncable returns the folders this device may pull from the peer: those both
// sides have, that this side receives into and that are not paused.
func (l *peerLoop) syncable(ctx context.Context, conn ports.PeerConn, only folder.ID) []folder.Folder {
	advertised := make(map[folder.ID]struct{})
	for _, advert := range conn.RemoteHello().Folders {
		if advert.Mode != folder.ModeReceiveOnly {
			advertised[advert.ID] = struct{}{}
		}
	}

	configured, err := l.daemon.deps.Store.ListFolders(ctx)
	if err != nil {
		if ctx.Err() == nil {
			l.daemon.deps.Log.Error("load folders for sync", "error", err)
		}
		return nil
	}

	var out []folder.Folder
	for _, f := range configured {
		if only != "" && f.ID != only {
			continue
		}
		if _, shared := advertised[f.ID]; !shared || f.Paused || !f.CanReceive() {
			continue
		}
		out = append(out, f)
	}
	return out
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
