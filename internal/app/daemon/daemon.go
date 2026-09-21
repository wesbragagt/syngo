// Package daemon keeps folder metadata current: it watches folders for
// changes, rescans them periodically and answers control requests.
package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/repair"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// Default timings used when a config field is zero.
const (
	DefaultRescanInterval = 10 * time.Minute
	DefaultDebounce       = 500 * time.Millisecond
)

// Scanner scans a folder; *scan.Scanner implements it.
type Scanner interface {
	Scan(ctx context.Context, f folder.Folder, o scan.Options) (scan.Result, error)
}

// Repairer restores consistency for one folder; *repair.Service implements it.
type Repairer interface {
	Run(ctx context.Context, id folder.ID, dryRun bool) (repair.Report, error)
}

// Config holds the daemon's runtime settings.
type Config struct {
	// RescanInterval is how often every folder is fully rescanned. It is
	// also how often the folder table is re-read, so folders added or
	// removed while the daemon runs are picked up.
	RescanInterval time.Duration
	// Debounce is how long a burst of filesystem events is collected before
	// the affected subtrees are scanned.
	Debounce time.Duration
	// Listen is the address the daemon reports in status. The listener is
	// bound by the composition root, which passes the bound address here.
	Listen string
	// PeerPingInterval is how often a connected peer is pinged.
	PeerPingInterval time.Duration
	// PID is the process id to report in status. The application layer has
	// no access to the process itself, so the composition root fills it in.
	PID int
}

// rescanInterval returns the configured interval or the default.
func (c Config) rescanInterval() time.Duration {
	if c.RescanInterval > 0 {
		return c.RescanInterval
	}
	return DefaultRescanInterval
}

// peerPingInterval returns the configured peer ping interval or the default.
func (c Config) peerPingInterval() time.Duration {
	if c.PeerPingInterval > 0 {
		return c.PeerPingInterval
	}
	return DefaultPeerPingInterval
}

// debounce returns the configured debounce or the default.
func (c Config) debounce() time.Duration {
	if c.Debounce > 0 {
		return c.Debounce
	}
	return DefaultDebounce
}

// Deps are the collaborators the daemon drives.
type Deps struct {
	Store   ports.MetadataStore
	Clock   ports.Clock
	Watcher ports.Watcher
	Scanner Scanner
	Repair  Repairer
	Status  *status.Service
	// Control serves the control socket. A nil server disables it.
	Control ports.ControlServer
	// Listener accepts peer connections. A nil listener disables them.
	Listener ports.Listener
	// Peers answers what connected peers ask of this device. It is
	// required when Listener is set.
	Peers ports.PeerHandler
	// Dialer connects to known peers. A nil dialer disables outgoing
	// connections, so peers are reported as not connected.
	Dialer ports.Dialer
	Log    *slog.Logger
}

// Daemon owns one folder loop per configured folder and the control server.
type Daemon struct {
	cfg  Config
	deps Deps

	ready     chan struct{}
	readyOnce sync.Once
	startedAt time.Time

	mu         sync.Mutex
	loops      map[folder.ID]*folderLoop
	states     map[folder.ID]string
	peers      map[device.ID]*peerLoop
	peerStates map[device.ID]status.PeerLive
	cancel     context.CancelFunc
}

// New returns a daemon. Run starts it.
func New(cfg Config, d Deps) *Daemon {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Daemon{
		cfg:        cfg,
		deps:       d,
		ready:      make(chan struct{}),
		loops:      make(map[folder.ID]*folderLoop),
		states:     make(map[folder.ID]string),
		peers:      make(map[device.ID]*peerLoop),
		peerStates: make(map[device.ID]status.PeerLive),
	}
}

// Ready is closed once the control server and the initial folder loops are up.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

// Run drives the daemon until ctx is done or a control client asks it to shut
// down. It returns nil after a clean stop: folder loops are cancelled, the
// in-flight scan is abandoned and the control socket is closed.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	d.mu.Lock()
	d.cancel = cancel
	d.startedAt = d.deps.Clock.Now()
	d.mu.Unlock()

	var control sync.WaitGroup
	if d.deps.Control != nil {
		control.Add(1)
		go func() {
			defer control.Done()
			if err := d.deps.Control.Serve(ctx, d); err != nil {
				d.deps.Log.Error("control server stopped", "error", err)
			}
		}()
	}

	var listener sync.WaitGroup
	if d.deps.Listener != nil && d.deps.Peers != nil {
		listener.Add(1)
		go func() {
			defer listener.Done()
			if err := d.deps.Listener.Serve(ctx, d.deps.Peers, d.knownDevice); err != nil {
				d.deps.Log.Error("peer listener stopped", "error", err)
			}
		}()
	}

	if err := d.reload(ctx); err != nil {
		d.deps.Log.Error("load folders", "error", err)
	}
	d.readyOnce.Do(func() { close(d.ready) })
	d.deps.Log.Info("daemon started", "folders", d.loopCount(), "rescan_interval", d.cfg.rescanInterval())

	ticker := time.NewTicker(d.cfg.rescanInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.stop()
			if d.deps.Control != nil {
				if err := d.deps.Control.Close(); err != nil {
					d.deps.Log.Error("close control server", "error", err)
				}
			}
			control.Wait()
			listener.Wait()
			d.deps.Log.Info("daemon stopped")
			return nil
		case <-ticker.C:
			if err := d.reload(ctx); err != nil {
				d.deps.Log.Error("load folders", "error", err)
			}
		}
	}
}

// reload starts a loop for every folder that does not have one and stops the
// loops of folders that disappeared from the configuration.
func (d *Daemon) reload(ctx context.Context) error {
	configured, err := d.deps.Store.ListFolders(ctx)
	if err != nil {
		return err
	}

	seen := make(map[folder.ID]struct{}, len(configured))
	for _, f := range configured {
		seen[f.ID] = struct{}{}
		d.mu.Lock()
		_, running := d.loops[f.ID]
		if !running {
			loop := newFolderLoop(d, f)
			d.loops[f.ID] = loop
			loop.start(ctx)
		}
		d.mu.Unlock()
		if !running {
			d.deps.Log.Info("watching folder", "folder", f.ID, "path", f.Path)
		}
	}

	d.mu.Lock()
	var gone []*folderLoop
	for id, loop := range d.loops {
		if _, ok := seen[id]; !ok {
			gone = append(gone, loop)
			delete(d.loops, id)
			delete(d.states, id)
		}
	}
	d.mu.Unlock()

	for _, loop := range gone {
		loop.stop()
		d.deps.Log.Info("stopped watching folder", "folder", loop.folder.ID)
	}
	return d.reloadPeers(ctx)
}

// reloadPeers starts a loop for every known peer that does not have one, and
// stops the loops of peers that were removed. A peer whose address changed is
// reconnected.
func (d *Daemon) reloadPeers(ctx context.Context) error {
	if d.deps.Dialer == nil {
		return nil
	}
	known, err := d.deps.Store.ListDevices(ctx)
	if err != nil {
		return err
	}

	seen := make(map[device.ID]struct{}, len(known))
	var stale []*peerLoop
	d.mu.Lock()
	for _, peer := range known {
		seen[peer.ID] = struct{}{}
		if running, ok := d.peers[peer.ID]; ok {
			if running.peer.Address == peer.Address {
				continue
			}
			stale = append(stale, running)
			delete(d.peers, peer.ID)
		}
		loop := newPeerLoop(d, peer)
		d.peers[peer.ID] = loop
		loop.start(ctx)
	}
	for id, loop := range d.peers {
		if _, ok := seen[id]; !ok {
			stale = append(stale, loop)
			delete(d.peers, id)
		}
	}
	d.mu.Unlock()

	for _, loop := range stale {
		loop.stop()
	}
	return nil
}

// knownDevice reports whether a device may connect to us.
func (d *Daemon) knownDevice(ctx context.Context, id device.ID) bool {
	if _, err := d.deps.Store.GetDevice(ctx, id); err != nil {
		return false
	}
	return true
}

// stop cancels every folder loop and waits for it to return.
func (d *Daemon) stop() {
	d.mu.Lock()
	loops := make([]*folderLoop, 0, len(d.loops))
	for id, loop := range d.loops {
		loops = append(loops, loop)
		delete(d.loops, id)
	}
	d.mu.Unlock()

	d.mu.Lock()
	peers := make([]*peerLoop, 0, len(d.peers))
	for id, loop := range d.peers {
		peers = append(peers, loop)
		delete(d.peers, id)
	}
	d.mu.Unlock()

	for _, loop := range loops {
		loop.stop()
	}
	for _, loop := range peers {
		loop.stop()
	}
}

// loopCount returns how many folder loops are running.
func (d *Daemon) loopCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.loops)
}

// setState records the live state of a folder, as reported by status. An empty
// state means the folder is no longer busy.
func (d *Daemon) setState(id folder.ID, state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if state == "" {
		delete(d.states, id)
		return
	}
	d.states[id] = state
}

// setPeerState records what the peer loop knows about a peer. A nil state
// forgets it.
func (d *Daemon) setPeerState(id device.ID, state *status.PeerLive) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if state == nil {
		delete(d.peerStates, id)
		return
	}
	d.peerStates[id] = *state
}

// live returns what only a running daemon knows.
func (d *Daemon) live() *status.LiveInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	states := make(map[string]string, len(d.states))
	for id, state := range d.states {
		states[string(id)] = state
	}
	peers := make(map[string]status.PeerLive, len(d.peerStates))
	for id, state := range d.peerStates {
		peers[string(id)] = state
	}
	return &status.LiveInfo{
		PID:          d.cfg.PID,
		Listen:       d.cfg.Listen,
		StartedAt:    d.startedAt,
		Peers:        peers,
		FolderStates: states,
	}
}

// loop returns the running loop of a folder.
func (d *Daemon) loop(id folder.ID) (*folderLoop, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	loop, ok := d.loops[id]
	return loop, ok
}

// loopsSnapshot returns every running folder loop.
func (d *Daemon) loopsSnapshot() []*folderLoop {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*folderLoop, 0, len(d.loops))
	for _, loop := range d.loops {
		out = append(out, loop)
	}
	return out
}
