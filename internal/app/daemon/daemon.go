// Package daemon keeps folder metadata current: it watches folders for
// changes, rescans them periodically and answers control requests.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/network"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/repair"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// Default timings used when a config field is zero.
const (
	DefaultRescanInterval = 10 * time.Minute
	DefaultDebounce       = 500 * time.Millisecond
	DefaultSyncInterval   = 30 * time.Second
	// DefaultPathInterval is how often the local Tailscale client is asked
	// which path each peer is reached over. It is read on a timer rather
	// than per request so a status call never waits for the client.
	DefaultPathInterval = 30 * time.Second
)

// PathProbe reports how traffic reaches each of the given devices.
// *network.Service implements it.
type PathProbe interface {
	PeerPaths(ctx context.Context, devices []device.Device) (map[device.ID]ports.TailnetPath, error)
}

// Scanner scans a folder; *scan.Scanner implements it.
type Scanner interface {
	Scan(ctx context.Context, f folder.Folder, o scan.Options) (scan.Result, error)
}

// Repairer restores consistency for one folder; *repair.Service implements it.
type Repairer interface {
	Run(ctx context.Context, id folder.ID, dryRun bool) (repair.Report, error)
}

// Syncer pulls from a peer what a folder is missing; *syncer.Puller
// implements it.
type Syncer interface {
	SyncFolder(ctx context.Context, conn ports.PeerConn, f folder.Folder) (syncer.Summary, error)
	// ResumeAll prepares the unfinished transfers of a folder after a
	// restart.
	ResumeAll(ctx context.Context, f folder.Folder) error
}

// Config holds the daemon's runtime settings.
type Config struct {
	// RescanInterval is how often every folder is fully rescanned. It is
	// also how often the folder and device tables are re-read, as a
	// backstop: a CLI command that changes them asks the daemon to reload
	// right away.
	RescanInterval time.Duration
	// Debounce is how long a burst of filesystem events is collected before
	// the affected subtrees are scanned.
	Debounce time.Duration
	// Listen is the address the daemon reports in status. The listener is
	// bound by the composition root, which passes the bound address here.
	Listen string
	// PeerPingInterval is how often a connected peer is pinged.
	PeerPingInterval time.Duration
	// SyncInterval is how often a connected peer is asked what changed on
	// its side, on top of the change notifications it sends.
	SyncInterval time.Duration
	// PathInterval is how often the local Tailscale client is asked which
	// path each peer is reached over; zero means DefaultPathInterval.
	PathInterval time.Duration
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

// syncInterval returns the configured sync interval or the default.
func (c Config) syncInterval() time.Duration {
	if c.SyncInterval > 0 {
		return c.SyncInterval
	}
	return DefaultSyncInterval
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
	// Syncer pulls folder contents from connected peers. A nil syncer
	// leaves the daemon watching folders without synchronizing them.
	Syncer Syncer
	// ListenState reports how the peer port is bound and what it has been
	// through. A nil function leaves the listener section of the status
	// report at what Config.Listen says.
	ListenState func() network.ListenState
	// Paths reports how traffic reaches each peer, read from the local
	// Tailscale client. A nil probe leaves every path unknown.
	Paths PathProbe
	// Settings is where the runtime settings are re-read from on start and
	// on reload. A nil store leaves the limits as they were wired.
	Settings ports.SettingsStore
	// Upload and Download pace what this device serves and asks for. A nil
	// limiter means that direction is unlimited.
	Upload, Download ports.Limiter
	Log              *slog.Logger
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
	peerPaths  map[device.ID]status.PathLive
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
		peerPaths:  make(map[device.ID]status.PathLive),
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

	if err := d.refresh(ctx); err != nil {
		d.deps.Log.Error("load folders", "error", err)
	}
	if err := d.applySettings(ctx); err != nil {
		d.deps.Log.Error("load settings", "error", err)
	}
	var paths sync.WaitGroup
	if d.deps.Paths != nil {
		paths.Add(1)
		go func() {
			defer paths.Done()
			d.watchPaths(ctx)
		}()
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
			paths.Wait()
			d.deps.Log.Info("daemon stopped")
			return nil
		case <-ticker.C:
			if err := d.refresh(ctx); err != nil {
				d.deps.Log.Error("load folders", "error", err)
			}
		}
	}
}

// refresh starts a loop for every folder that does not have one, restarts the
// loop of a folder whose configuration moved out from under it, and stops the
// loops of folders that disappeared from the configuration. It then does the
// same for peers.
func (d *Daemon) refresh(ctx context.Context) error {
	configured, err := d.deps.Store.ListFolders(ctx)
	if err != nil {
		return err
	}

	var (
		stale   []*folderLoop
		gone    []*folderLoop
		started []*folderLoop
	)
	seen := make(map[folder.ID]struct{}, len(configured))
	d.mu.Lock()
	for _, f := range configured {
		seen[f.ID] = struct{}{}
		running, ok := d.loops[f.ID]
		if ok {
			if !loopSettingsChanged(running.folder, f) {
				continue
			}
			stale = append(stale, running)
		}
		loop := newFolderLoop(d, f)
		d.loops[f.ID] = loop
		started = append(started, loop)
	}
	for id, loop := range d.loops {
		if _, ok := seen[id]; !ok {
			gone = append(gone, loop)
			delete(d.loops, id)
			delete(d.states, id)
		}
	}
	d.mu.Unlock()

	// The old loops stop before the new ones start: one still watching the
	// previous path would keep reporting events for a directory the folder
	// no longer has.
	for _, loop := range stale {
		loop.stop()
		d.deps.Log.Info("folder configuration changed, watching it again",
			"folder", loop.folder.ID, "path", loop.folder.Path)
	}
	for _, loop := range gone {
		loop.stop()
		d.deps.Log.Info("stopped watching folder", "folder", loop.folder.ID)
	}
	for _, loop := range started {
		loop.start(ctx)
		d.deps.Log.Info("watching folder", "folder", loop.folder.ID, "path", loop.folder.Path)
	}
	return d.reloadPeers(ctx)
}

// loopSettingsChanged reports whether a folder was reconfigured in a way its
// loop cannot pick up on its own. The loop re-reads the folder before every
// scan, so the mode, the policies and the ignore patterns need no restart; the
// path it watches and the settings the watch itself is built from do.
func loopSettingsChanged(running, stored folder.Folder) bool {
	return running.Path != stored.Path ||
		running.Symlinks != stored.Symlinks ||
		running.IncludeHidden != stored.IncludeHidden
}

// reloadPeers starts a loop for every known peer that does not have one, stops
// the loops of peers that were removed and drops the connections those peers
// opened towards us. A peer whose address changed is reconnected.
func (d *Daemon) reloadPeers(ctx context.Context) error {
	known, err := d.deps.Store.ListDevices(ctx)
	if err != nil {
		return err
	}
	if d.deps.Listener != nil {
		// A peer that was removed may still hold a connection it
		// opened itself, which no outgoing loop owns.
		ids := make([]device.ID, 0, len(known))
		for _, peer := range known {
			ids = append(ids, peer.ID)
		}
		d.deps.Listener.RetainDevices(ids)
	}
	if d.deps.Dialer == nil {
		return nil
	}

	seen := make(map[device.ID]struct{}, len(known))
	var (
		stale   []*peerLoop
		started []*peerLoop
	)
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
		started = append(started, loop)
	}
	for id, loop := range d.peers {
		if _, ok := seen[id]; !ok {
			stale = append(stale, loop)
			delete(d.peers, id)
		}
	}
	d.mu.Unlock()

	// The stale loop goes first: stopping it clears the peer's reported
	// state, which would otherwise wipe what the new loop just reported.
	for _, loop := range stale {
		loop.stop()
	}
	for _, loop := range started {
		loop.start(ctx)
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

// setPeerState records what the peer loop knows about a peer.
func (d *Daemon) setPeerState(id device.ID, state *status.PeerLive) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peerStates[id] = *state
}

// forgetPeerState drops what a stopping loop reported, unless another loop has
// taken the peer over in the meantime. A peer whose address changed gets a new
// loop while the old one is still stopping, and what the new one reports must
// not be wiped by the old one's last act.
func (d *Daemon) forgetPeerState(id device.ID, loop *peerLoop) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current, ok := d.peers[id]; ok && current != loop {
		return
	}
	delete(d.peerStates, id)
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
	for id, state := range peers {
		if path, ok := d.peerPaths[device.ID(id)]; ok {
			state.Path = path
			peers[id] = state
		}
	}
	info := &status.LiveInfo{
		PID:          d.cfg.PID,
		Listen:       d.listenAddress(),
		Listener:     d.listenerSection(),
		StartedAt:    d.startedAt,
		Peers:        peers,
		FolderStates: states,
	}
	if d.deps.Upload != nil {
		info.UploadLimit = d.deps.Upload.Rate()
	}
	if d.deps.Download != nil {
		info.DownloadLimit = d.deps.Download.Rate()
	}
	return info
}

// listenAddress is the address peers connect to: the one the listener is bound
// to when there is one, else what the run was configured with.
func (d *Daemon) listenAddress() string {
	if d.deps.ListenState == nil {
		return d.cfg.Listen
	}
	if addr := d.deps.ListenState().Address; addr != "" {
		return addr
	}
	return d.cfg.Listen
}

// listenerSection reports how the peer port is bound. Without a listener that
// can rebind, the daemon only knows that it asked for an address, so that is
// all it says.
func (d *Daemon) listenerSection() status.Listener {
	if d.deps.ListenState == nil {
		return status.Listener{Bound: d.deps.Listener != nil, Address: d.cfg.Listen}
	}
	state := d.deps.ListenState()
	return status.Listener{
		Bound:     state.Bound,
		Address:   state.Address,
		Interface: state.Interface,
		BoundAt:   rfc3339(state.BoundAt),
		LostAt:    rfc3339(state.LostAt),
		Rebinds:   state.Rebinds,
		Failures:  state.Failures,
		Error:     nonEmpty(state.Error),
	}
}

// rfc3339 formats a time as an RFC3339 UTC string, or nil if it is zero.
func rfc3339(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// nonEmpty returns a pointer to s, or nil when s is empty.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// watchPaths asks the local Tailscale client which path each peer is reached
// over, now and on every tick. A client that cannot answer leaves every path
// unknown with the reason, which is what status then reports: nothing here
// ever guesses a path.
func (d *Daemon) watchPaths(ctx context.Context) {
	interval := d.cfg.PathInterval
	if interval <= 0 {
		interval = DefaultPathInterval
	}
	d.refreshPaths(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshPaths(ctx)
		}
	}
}

// refreshPaths reads the path of every known peer once.
func (d *Daemon) refreshPaths(ctx context.Context) {
	devices, err := d.deps.Store.ListDevices(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.deps.Log.Debug("load devices for path diagnostics", "error", err)
		}
		return
	}
	paths, err := d.deps.Paths.PeerPaths(ctx, devices)
	if err != nil {
		d.deps.Log.Debug("read tailnet paths", "error", err)
	}
	now := d.deps.Clock.Now()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.peerPaths = make(map[device.ID]status.PathLive, len(paths))
	for id, path := range paths {
		d.peerPaths[id] = status.PathLive{
			Kind: string(path.Kind), Endpoint: path.Endpoint, Relay: path.Relay,
			Active: path.Active, Reason: path.Reason, ObservedAt: now,
		}
	}
}

// applySettings re-reads the runtime settings and hands the limits to the
// limiters, so a rate written while the daemon runs takes effect without
// reconnecting anything.
func (d *Daemon) applySettings(ctx context.Context) error {
	if d.deps.Settings == nil {
		return nil
	}
	settings, err := d.deps.Settings.Load(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if d.deps.Upload != nil {
		d.deps.Upload.SetRate(settings.UploadLimit)
	}
	if d.deps.Download != nil {
		d.deps.Download.SetRate(settings.DownloadLimit)
	}
	d.deps.Log.Info("bandwidth limits applied",
		"upload", settings.UploadLimit.String(), "download", settings.DownloadLimit.String())
	return nil
}

// peersSnapshot returns every running peer loop.
func (d *Daemon) peersSnapshot() []*peerLoop {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*peerLoop, 0, len(d.peers))
	for _, loop := range d.peers {
		out = append(out, loop)
	}
	return out
}

// loop returns the running loop of a folder.
func (d *Daemon) loop(id folder.ID) (*folderLoop, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	loop, ok := d.loops[id]
	return loop, ok
}
