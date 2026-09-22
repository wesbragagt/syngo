package network

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/backoff"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// DefaultHoldCheck is how often the bound address is checked for still being
// configured on this machine. A listening socket outlives the interface under
// it, so nothing reports the loss by itself; something has to look.
const DefaultHoldCheck = 2 * time.Second

// ListenState is what the daemon reports about the port peers connect to.
// Every time is the clock the rebinder was given, so a test controls them.
type ListenState struct {
	// Bound reports whether a socket is accepting connections now.
	Bound bool
	// Address is the address bound now, or the last one that was bound.
	Address string
	// BoundAt is when the current socket was bound. It is zero before the
	// first bind.
	BoundAt time.Time
	// LostAt is when the address was last lost. It is zero while nothing
	// has been lost, and keeps its value after a rebind, so status can say
	// what happened.
	LostAt time.Time
	// Rebinds counts how many times the listener came back after losing
	// its address.
	Rebinds int
	// Failures counts the consecutive bind attempts that failed. It is
	// zero while the listener is bound.
	Failures int
	// Interface is the interface name the address is taken from, empty
	// when the address was configured directly.
	Interface string
	// Error is why the last bind attempt failed. It is empty while the
	// listener is bound.
	Error string
}

// RebinderConfig tunes how hard a listener tries to come back.
type RebinderConfig struct {
	// HoldCheck is how often the bound address is checked; zero means
	// DefaultHoldCheck.
	HoldCheck time.Duration
	// Backoff bounds the wait between bind attempts. Its zero value never
	// spins and never waits longer than backoff.DefaultMax.
	Backoff backoff.Policy
	// Interface is the interface name the address is taken from, reported
	// as it is; it is empty when the address was configured directly.
	Interface string
	Clock     ports.Clock
	Log       *slog.Logger
}

// Rebinder is a ports.Listener that owns its socket and binds it again every
// time the address goes away: a Tailscale client restarting under the daemon,
// an interface renumbered, a link that drops, a machine that slept. The daemon
// above it sees one listener that is sometimes not accepting, and never has to
// be restarted.
type Rebinder struct {
	binder ports.Binder
	cfg    RebinderConfig

	mu      sync.Mutex
	current ports.BoundListener
	state   ListenState
}

var _ ports.Listener = (*Rebinder)(nil)

// Bind binds the peer port once and returns a listener that keeps it bound.
// The first bind happens here rather than in Serve so a misconfigured address
// stops the daemon from starting, and so the caller can report the port a
// ":0" bind chose.
func Bind(binder ports.Binder, cfg RebinderConfig) (*Rebinder, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.HoldCheck <= 0 {
		cfg.HoldCheck = DefaultHoldCheck
	}
	r := &Rebinder{binder: binder, cfg: cfg}
	r.state.Interface = cfg.Interface

	addr, err := binder.Resolve()
	if err != nil {
		return nil, err
	}
	ln, err := binder.Bind(addr)
	if err != nil {
		return nil, err
	}
	r.adopt(ln, false)
	return r, nil
}

// Addr returns the address bound now, or the last one that was bound. It keeps
// reporting that address while the listener is rebinding, because it is what
// peers are configured with and what the daemon comes back on.
func (r *Rebinder) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Address
}

// State returns what the listener has been through, for status.
func (r *Rebinder) State() ListenState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Serve accepts peer connections until ctx is done, rebinding whenever the
// address goes away. It returns nil on a clean stop: a bind that fails is a
// condition to wait out, not a reason to stop serving.
func (r *Rebinder) Serve(ctx context.Context, h ports.PeerHandler, known ports.DeviceLookup) error {
	for ctx.Err() == nil {
		ln := r.take()
		if ln == nil {
			var err error
			if ln, err = r.rebind(ctx); err != nil {
				return nil
			}
		}
		r.serveOne(ctx, ln, h, known)
	}
	r.closeCurrent()
	return nil
}

// serveOne serves one bound socket until it stops accepting, and records the
// loss. The address is watched while it serves: a socket whose interface went
// away keeps accepting nothing for ever, so waiting for Serve to fail would
// wait for ever.
func (r *Rebinder) serveOne(ctx context.Context, ln ports.BoundListener, h ports.PeerHandler, known ports.DeviceLookup) {
	served, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go r.watch(served, ln)

	err := ln.Serve(ctx, h, known)
	_ = ln.Close()
	if ctx.Err() != nil {
		return
	}
	r.lost(err)
}

// watch closes the socket once the machine no longer has its address, which is
// what makes Serve return and the rebind start.
func (r *Rebinder) watch(ctx context.Context, ln ports.BoundListener) {
	addr := ln.Addr()
	ticker := time.NewTicker(r.cfg.HoldCheck)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.binder.Held(addr) {
				continue
			}
			r.cfg.Log.Warn("listen address is gone, rebinding", "address", addr)
			_ = ln.Close()
			return
		}
	}
}

// rebind tries until it binds or ctx ends, waiting a bounded time between
// attempts so an address that stays gone costs one attempt per interval.
func (r *Rebinder) rebind(ctx context.Context) (ports.BoundListener, error) {
	for {
		addr, err := r.binder.Resolve()
		if err == nil {
			var ln ports.BoundListener
			if ln, err = r.binder.Bind(addr); err == nil {
				r.adopt(ln, true)
				r.cfg.Log.Info("listener rebound", "address", ln.Addr())
				return ln, nil
			}
		}

		wait := r.failed(err)
		r.cfg.Log.Warn("binding the peer port failed", "error", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// adopt records a socket as the one in use.
func (r *Rebinder) adopt(ln ports.BoundListener, rebound bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = ln
	r.state.Bound = true
	r.state.Address = ln.Addr()
	r.state.BoundAt = r.now()
	r.state.Failures = 0
	r.state.Error = ""
	if rebound {
		r.state.Rebinds++
	}
}

// take returns the socket in use, or nil while the listener is rebinding.
func (r *Rebinder) take() ports.BoundListener {
	r.mu.Lock()
	defer r.mu.Unlock()
	ln := r.current
	return ln
}

// lost records that the bound socket stopped serving.
func (r *Rebinder) lost(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = nil
	r.state.Bound = false
	r.state.LostAt = r.now()
	if err != nil {
		r.state.Error = err.Error()
	}
	r.cfg.Log.Warn("peer listener lost its address", "address", r.state.Address, "error", err)
}

// failed records a bind attempt that did not work and returns how long to wait
// before the next one.
func (r *Rebinder) failed(err error) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Failures++
	if err != nil {
		r.state.Error = err.Error()
	}
	return r.cfg.Backoff.Wait(r.state.Failures)
}

// closeCurrent releases the socket held outside Serve, which is the one Bind
// made for a daemon that never called Serve.
func (r *Rebinder) closeCurrent() {
	r.mu.Lock()
	ln := r.current
	r.current = nil
	r.state.Bound = false
	r.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// Close releases the socket. It is what the composition root calls when the
// daemon it built never ran.
func (r *Rebinder) Close() error {
	r.closeCurrent()
	return nil
}

// now reads the clock, or the wall clock when none was given.
func (r *Rebinder) now() time.Time {
	if r.cfg.Clock == nil {
		return time.Now()
	}
	return r.cfg.Clock.Now()
}

// Broadcast tells every connected peer that a folder changed. It does nothing
// while the listener is rebinding: there is nobody connected to tell.
func (r *Rebinder) Broadcast(id folder.ID, seq uint64) {
	if ln := r.take(); ln != nil {
		ln.Broadcast(id, seq)
	}
}

// AnnounceFolders tells every connected peer which folders this device serves.
func (r *Rebinder) AnnounceFolders(adverts []ports.FolderAdvert) {
	if ln := r.take(); ln != nil {
		ln.AnnounceFolders(adverts)
	}
}

// RetainDevices closes the connections of peers that are no longer known.
func (r *Rebinder) RetainDevices(ids []device.ID) {
	if ln := r.take(); ln != nil {
		ln.RetainDevices(ids)
	}
}
