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

// DefaultSettle is how long a socket has to serve before the listener counts
// as healthy again. A socket that serves longer than this and then stops has
// run into something that happened to it, and is rebound at once; one that
// stops sooner is failing as fast as it is bound, and is spaced out by the
// backoff like any other failure.
const DefaultSettle = time.Second

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
	// Settle is how long a socket has to serve before the listener counts
	// as healthy again; zero means DefaultSettle.
	Settle time.Duration
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
	// shortCycles counts the sockets in a row that stopped serving before
	// they had settled. It is what spaces the loop, and unlike
	// state.Failures a successful bind does not reset it: a bind that
	// succeeds and then stops at once has achieved nothing.
	shortCycles int
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
	if cfg.Settle <= 0 {
		cfg.Settle = DefaultSettle
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
//
// Two things can end a cycle, and both are spaced by the same backoff: a bind
// that failed, and a socket that was bound and stopped serving. They are told
// apart by how long the socket served, not by why it stopped. A socket that
// served longer than the settle time was healthy, so whatever happened to it
// is a new event and it is bound again at once; one that stopped sooner is
// failing as fast as it is bound, and waiting is the only thing that keeps the
// loop from spinning.
func (r *Rebinder) Serve(ctx context.Context, h ports.PeerHandler, known ports.DeviceLookup) error {
	for ctx.Err() == nil {
		ln := r.take()
		if ln == nil {
			var err error
			if ln, err = r.rebind(ctx); err != nil {
				return nil
			}
		}

		start := time.Now()
		r.serveOne(ctx, ln, h, known)
		if ctx.Err() != nil {
			break
		}
		if wait := r.cycleEnded(time.Since(start)); wait > 0 {
			r.cfg.Log.Warn("the peer listener stopped serving as soon as it was bound",
				"served_for", time.Since(start), "retry_in", wait)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
		}
	}
	r.closeCurrent()
	return nil
}

// cycleEnded records a socket that stopped serving and returns how long to
// wait before binding again: nothing when it had served long enough to count
// as healthy, and the backoff when it had not.
//
// The elapsed time is measured with the wall clock rather than the injected
// one: it decides how fast this loop may run, which is a property of the
// machine and not of what a test moved the clock to.
func (r *Rebinder) cycleEnded(served time.Duration) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if served >= r.cfg.Settle {
		r.shortCycles = 0
		return 0
	}
	r.shortCycles++
	return r.cfg.Backoff.Wait(r.shortCycles)
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
