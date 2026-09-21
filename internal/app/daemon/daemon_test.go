package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/daemon"
	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/repair"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/app/status"
	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// folderRoot is the path the test folder claims to live at. Nothing touches
// the filesystem: the scanner is a fake.
const folderRoot = "/srv/docs"

// scanCall records one invocation of the fake scanner.
type scanCall struct {
	ID    folder.ID
	Paths []string
}

// full reports whether the call scanned the whole folder.
func (c scanCall) full() bool { return len(c.Paths) == 0 }

// recordingScanner is a daemon.Scanner that records its calls and can be told
// to block, so a test can observe a scan in progress.
type recordingScanner struct {
	mu      sync.Mutex
	calls   []scanCall
	release chan struct{}
	started chan struct{}
	err     error
}

// newRecordingScanner returns a scanner that returns immediately.
func newRecordingScanner() *recordingScanner {
	return &recordingScanner{started: make(chan struct{}, 16)}
}

// Scan records the call and blocks while a release channel is installed.
func (s *recordingScanner) Scan(ctx context.Context, f folder.Folder, o scan.Options) (scan.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, scanCall{ID: f.ID, Paths: o.Paths})
	release := s.release
	scanErr := s.err
	s.mu.Unlock()

	select {
	case s.started <- struct{}{}:
	default:
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return scan.Result{}, ctx.Err()
		}
	}
	if scanErr != nil {
		return scan.Result{}, scanErr
	}
	return scan.Result{FolderID: f.ID}, nil
}

// failWith makes every scan fail with err.
func (s *recordingScanner) failWith(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// blockUntil makes every scan wait for the returned channel to be closed.
func (s *recordingScanner) blockUntil() chan struct{} {
	release := make(chan struct{})
	s.mu.Lock()
	s.release = release
	s.mu.Unlock()
	return release
}

// unblock lets scans return immediately again.
func (s *recordingScanner) unblock() {
	s.mu.Lock()
	s.release = nil
	s.mu.Unlock()
}

// snapshot returns the calls recorded so far.
func (s *recordingScanner) snapshot() []scanCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scanCall(nil), s.calls...)
}

// folderAnnouncer is a ports.PeerHandler that only answers what this device
// announces: the folders in the store that are not paused, the way
// syncer.Server does. Nothing in these tests serves content.
type folderAnnouncer struct {
	store *fakes.Store
}

// Hello announces the folders the store holds.
func (h folderAnnouncer) Hello(ctx context.Context) ports.HelloInfo {
	info := ports.HelloInfo{DeviceID: "DEVICE", Name: "test", Folders: []ports.FolderAdvert{}}
	configured, err := h.store.ListFolders(ctx)
	if err != nil {
		return info
	}
	for _, f := range configured {
		if f.Paused {
			continue
		}
		info.Folders = append(info.Folders, ports.FolderAdvert{ID: f.ID, Mode: f.Mode})
	}
	return info
}

// Manifest serves nothing.
func (folderAnnouncer) Manifest(context.Context, device.ID, folder.ID) (manifest.Manifest, error) {
	return manifest.Manifest{}, errors.New("this fake peer handler serves no manifests")
}

// Chunk serves nothing.
func (folderAnnouncer) Chunk(context.Context, device.ID, ports.ChunkRequest) (ports.ChunkData, error) {
	return ports.ChunkData{}, errors.New("this fake peer handler serves no chunks")
}

// recordingSyncer is a daemon.Syncer that records which folders it was asked
// to pull, and moves nothing.
type recordingSyncer struct {
	mu     sync.Mutex
	synced []folder.ID
}

// SyncFolder records the folder and reports an empty cycle.
func (s *recordingSyncer) SyncFolder(_ context.Context, _ ports.PeerConn, f folder.Folder) (syncer.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced = append(s.synced, f.ID)
	return syncer.Summary{}, nil
}

// ResumeAll does nothing: no transfer exists in these tests.
func (s *recordingSyncer) ResumeAll(context.Context, folder.Folder) error { return nil }

// syncedFolders returns the folders SyncFolder was called with.
func (s *recordingSyncer) syncedFolders() []folder.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]folder.ID(nil), s.synced...)
}

// nopRepairer is a daemon.Repairer that reports nothing to fix.
type nopRepairer struct{}

// Run returns an empty report.
func (nopRepairer) Run(_ context.Context, id folder.ID, dryRun bool) (repair.Report, error) {
	return repair.Report{FolderID: id, DryRun: dryRun}, nil
}

// harness is a daemon running against fakes.
type harness struct {
	daemon   *daemon.Daemon
	store    *fakes.Store
	watcher  *fakes.Watcher
	scanner  *recordingScanner
	dialer   *fakes.Dialer
	listener *fakes.Listener
	syncer   *recordingSyncer
	done     chan error
	cancel   context.CancelFunc
	ctx      context.Context

	stopOnce sync.Once
	stopped  chan struct{}
	runErr   error
}

// wait returns what Run returned, or an error when it does not return in
// time. It may be called more than once.
func (h *harness) wait(timeout time.Duration) error {
	h.stopOnce.Do(func() {
		select {
		case h.runErr = <-h.done:
		case <-time.After(timeout):
			h.runErr = errors.New("daemon did not stop")
		}
		close(h.stopped)
	})
	<-h.stopped
	return h.runErr
}

// startDaemon wires a daemon around the given folders and runs it until the
// test ends.
func startDaemon(t *testing.T, cfg daemon.Config, folders ...folder.Folder) *harness {
	t.Helper()
	return startDaemonWithPeers(t, cfg, nil, folders...)
}

// startDaemonWithPeers wires a daemon that also knows the given peers. Their
// connections go through a fake dialer.
func startDaemonWithPeers(t *testing.T, cfg daemon.Config, peers []device.Device, folders ...folder.Folder) *harness {
	t.Helper()
	return startDaemonWatching(t, cfg, nil, peers, folders...)
}

// startDaemonWatching wires a daemon around a watcher of the test's choosing.
// A nil watcher installs the fake the other tests push events through.
func startDaemonWatching(t *testing.T, cfg daemon.Config, watcher ports.Watcher,
	peers []device.Device, folders ...folder.Folder) *harness {
	t.Helper()
	return startDaemonTweaked(t, cfg, watcher, nil, peers, folders...)
}

// startDaemonTweaked wires a daemon whose dependencies a test may add to, for
// the ports the other tests leave unwired.
func startDaemonTweaked(t *testing.T, cfg daemon.Config, watcher ports.Watcher,
	tweak func(*daemon.Deps), peers []device.Device, folders ...folder.Folder) *harness {
	t.Helper()

	store := fakes.NewStore()
	for _, f := range folders {
		if err := store.SaveFolder(context.Background(), f); err != nil {
			t.Fatalf("save folder: %v", err)
		}
	}
	for _, p := range peers {
		if err := store.SaveDevice(context.Background(), p); err != nil {
			t.Fatalf("save device: %v", err)
		}
	}
	dialer := fakes.NewDialer()
	listener := fakes.NewListener()
	pulls := &recordingSyncer{}
	clock := fakes.NewClock(time.Time{})
	identity := fakes.NewIdentity("DEVICE", "test")
	fakeWatcher := fakes.NewWatcher()
	if watcher == nil {
		watcher = fakeWatcher
	}
	scanner := newRecordingScanner()

	if cfg.RescanInterval == 0 {
		cfg.RescanInterval = time.Hour
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = 10 * time.Millisecond
	}
	deps := daemon.Deps{
		Store:    store,
		Clock:    clock,
		Watcher:  watcher,
		Scanner:  scanner,
		Repair:   nopRepairer{},
		Status:   status.New(store, identity, clock, "/data"),
		Dialer:   dialer,
		Listener: listener,
		Peers:    folderAnnouncer{store: store},
		Syncer:   pulls,
	}
	if tweak != nil {
		tweak(&deps)
	}
	d := daemon.New(cfg, deps)

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{daemon: d, store: store, watcher: fakeWatcher, scanner: scanner, dialer: dialer,
		listener: listener, syncer: pulls, done: make(chan error, 1), cancel: cancel, ctx: ctx, stopped: make(chan struct{})}
	go func() { h.done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := h.wait(5 * time.Second); err != nil {
			t.Errorf("daemon stop: %v", err)
		}
	})

	select {
	case <-d.Ready():
	case <-time.After(2 * time.Second):
		t.Fatalf("daemon not ready")
	}
	return h
}

// push delivers a watch event to the daemon.
func (h *harness) push(t *testing.T, ev ports.WatchEvent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 2*time.Second)
	defer cancel()
	if err := h.watcher.Push(ctx, ev); err != nil {
		t.Fatalf("push %v: %v", ev, err)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testFolder returns a folder configuration for the fakes. Each folder gets
// its own root, since the store refuses to configure a path twice.
func testFolder(id folder.ID, paused bool) folder.Folder {
	return folder.Folder{
		ID: id, Path: "/srv/" + string(id), Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkSkip,
		KeepEmptyDirs: true, Paused: paused,
	}
}

func TestDaemon_ScansEveryFolderOnStartup(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))

	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })
	if call := h.scanner.snapshot()[0]; !call.full() || call.ID != "docs" {
		t.Fatalf("startup call = %+v, want a full scan of docs", call)
	}
	// The watch is attached in its own goroutine, so it is waited for
	// rather than read once.
	waitFor(t, "the watch on the folder root", func() bool {
		roots := h.watcher.Roots()
		return len(roots) == 1 && roots[0] == folderRoot
	})
}

func TestDaemon_EventTriggersPartialScan(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	h.push(t, ports.WatchEvent{Path: folderRoot + "/sub/a.txt", Op: ports.OpWrite})

	waitFor(t, "partial scan", func() bool { return len(h.scanner.snapshot()) == 2 })
	call := h.scanner.snapshot()[1]
	if len(call.Paths) != 1 || call.Paths[0] != "sub" {
		t.Fatalf("scan paths = %v, want [sub]", call.Paths)
	}
}

func TestDaemon_BurstOfEventsBecomesOneScan(t *testing.T) {
	h := startDaemon(t, daemon.Config{Debounce: 50 * time.Millisecond}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	for i := range 100 {
		op := ports.OpWrite
		if i%3 == 0 {
			op = ports.OpCreate
		}
		h.push(t, ports.WatchEvent{Path: folderRoot + "/sub/a.txt", Op: op})
	}

	waitFor(t, "coalesced scan", func() bool { return len(h.scanner.snapshot()) == 2 })
	time.Sleep(150 * time.Millisecond)
	calls := h.scanner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("scans = %d, want 2 (startup plus one coalesced): %+v", len(calls), calls)
	}
	if len(calls[1].Paths) != 1 || calls[1].Paths[0] != "sub" {
		t.Fatalf("scan paths = %v, want [sub]", calls[1].Paths)
	}
}

func TestDaemon_OverflowForcesFullScan(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	h.push(t, ports.WatchEvent{Path: folderRoot + "/sub/a.txt", Op: ports.OpWrite})
	h.push(t, ports.WatchEvent{Path: folderRoot, Op: ports.OpOverflow})

	waitFor(t, "full scan", func() bool { return len(h.scanner.snapshot()) == 2 })
	if call := h.scanner.snapshot()[1]; !call.full() {
		t.Fatalf("scan paths = %v, want a full scan", call.Paths)
	}
}

func TestDaemon_PeriodicFullScan(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, testFolder("docs", false))

	waitFor(t, "periodic scans", func() bool { return len(h.scanner.snapshot()) >= 3 })
	for _, call := range h.scanner.snapshot() {
		if !call.full() {
			t.Fatalf("periodic call = %+v, want full scans only", call)
		}
	}
}

func TestDaemon_PausedFolderSkipsScans(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, testFolder("docs", true))

	h.push(t, ports.WatchEvent{Path: folderRoot + "/sub/a.txt", Op: ports.OpWrite})
	time.Sleep(150 * time.Millisecond)
	if calls := h.scanner.snapshot(); len(calls) != 0 {
		t.Fatalf("scans = %+v, want none while the folder is paused", calls)
	}

	// Resuming through the control API makes the folder catch up.
	if err := h.daemon.Resume(h.ctx, "docs"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitFor(t, "scan after resume", func() bool { return len(h.scanner.snapshot()) > 0 })
}

func TestDaemon_PicksUpNewFolderOnReload(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) >= 1 })

	if err := h.store.SaveFolder(h.ctx, testFolder("photos", false)); err != nil {
		t.Fatalf("save folder: %v", err)
	}

	waitFor(t, "scan of the new folder", func() bool {
		for _, call := range h.scanner.snapshot() {
			if call.ID == "photos" {
				return true
			}
		}
		return false
	})
}

func TestDaemon_DropsFolderRemovedFromConfig(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: 20 * time.Millisecond}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) >= 1 })

	if err := h.store.DeleteFolder(h.ctx, "docs"); err != nil {
		t.Fatalf("delete folder: %v", err)
	}
	waitFor(t, "loop shutdown", func() bool {
		before := len(h.scanner.snapshot())
		time.Sleep(60 * time.Millisecond)
		return len(h.scanner.snapshot()) == before
	})
}

func TestDaemon_StatusReportsScanningThenIdle(t *testing.T) {
	h := startDaemon(t, daemon.Config{PID: 4242, Listen: "127.0.0.1:22000"}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })
	release := h.scanner.blockUntil()

	h.push(t, ports.WatchEvent{Path: folderRoot + "/sub/a.txt", Op: ports.OpWrite})
	waitFor(t, "scanning state", func() bool { return folderState(t, h) == status.StateScanning })

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !report.Daemon.Running || report.Daemon.PID != 4242 || report.Daemon.Listen != "127.0.0.1:22000" {
		t.Fatalf("daemon section = %+v, want a running daemon with pid 4242", report.Daemon)
	}

	h.scanner.unblock()
	close(release)
	waitFor(t, "idle state", func() bool { return folderState(t, h) == status.StateIdle })
}

// folderState returns the reported state of the only folder.
func folderState(t *testing.T, h *harness) string {
	t.Helper()
	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(report.Folders) != 1 {
		t.Fatalf("folders = %d, want 1", len(report.Folders))
	}
	return report.Folders[0].State
}

func TestDaemon_CleanShutdownFromControl(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })
	release := h.scanner.blockUntil()
	h.push(t, ports.WatchEvent{Path: folderRoot + "/sub/a.txt", Op: ports.OpWrite})
	waitFor(t, "scan in flight", func() bool { return len(h.scanner.snapshot()) == 2 })

	if err := h.daemon.Shutdown(h.ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := h.wait(3 * time.Second); err != nil {
		t.Fatalf("Run() = %v, want a clean stop that cancels the in-flight scan", err)
	}
	close(release)
}

func TestDaemon_HandleUnknownCommand(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))

	resp := h.daemon.Handle(h.ctx, ports.ControlRequest{Cmd: "frobnicate"})
	if resp.OK {
		t.Fatalf("ok = true, want false for an unknown command")
	}
	if resp.Code != 2 {
		t.Fatalf("code = %d, want 2", resp.Code)
	}
}

func TestDaemon_HandlePauseAndResume(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))

	if resp := h.daemon.Handle(h.ctx, ports.ControlRequest{Cmd: daemon.CmdPause, FolderID: "docs"}); !resp.OK {
		t.Fatalf("pause failed: %s", resp.Error)
	}
	f, err := h.store.GetFolder(h.ctx, "docs")
	if err != nil {
		t.Fatalf("get folder: %v", err)
	}
	if !f.Paused {
		t.Fatalf("paused = false after pause")
	}

	if resp := h.daemon.Handle(h.ctx, ports.ControlRequest{Cmd: daemon.CmdResume, FolderID: "docs"}); !resp.OK {
		t.Fatalf("resume failed: %s", resp.Error)
	}
	if f, err = h.store.GetFolder(h.ctx, "docs"); err != nil || f.Paused {
		t.Fatalf("paused = %t after resume (err %v)", f.Paused, err)
	}
}

func TestDaemon_HandleUnknownFolderIsNotFound(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))

	resp := h.daemon.Handle(h.ctx, ports.ControlRequest{Cmd: daemon.CmdRescan, FolderID: "missing"})
	if resp.OK || resp.Code != 4 {
		t.Fatalf("response = %+v, want ok:false with code 4", resp)
	}
	if err := h.daemon.Rescan(h.ctx, "missing"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Rescan(missing) = %v, want ErrNotFound", err)
	}
}

func TestDaemon_HandleStatusResultDecodes(t *testing.T) {
	h := startDaemon(t, daemon.Config{PID: 7}, testFolder("docs", false))

	resp := h.daemon.Handle(h.ctx, ports.ControlRequest{Cmd: daemon.CmdStatus})
	if !resp.OK {
		t.Fatalf("status failed: %s", resp.Error)
	}
	var report status.Report
	if err := json.Unmarshal(resp.Result, &report); err != nil {
		t.Fatalf("decode status result: %v", err)
	}
	if report.Daemon.PID != 7 || len(report.Folders) != 1 {
		t.Fatalf("report = %+v, want pid 7 and one folder", report)
	}
}

func TestDaemon_ScanFailureLeavesFolderRecordToTheScanner(t *testing.T) {
	h := startDaemon(t, daemon.Config{}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	// The scanner is the only writer of the scan outcome, so whatever it
	// recorded must survive a failing scan in the loop.
	lastScan := time.Unix(1700000000, 0).UTC()
	if err := h.store.SetFolderScan(h.ctx, "docs", lastScan, ""); err != nil {
		t.Fatalf("SetFolderScan error: %v", err)
	}

	h.scanner.failWith(errors.New("boom"))
	if err := h.daemon.Rescan(h.ctx, "docs"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	waitFor(t, "failing rescan", func() bool { return len(h.scanner.snapshot()) > 1 })

	f, err := h.store.GetFolder(h.ctx, "docs")
	if err != nil {
		t.Fatalf("get folder: %v", err)
	}
	if !f.LastScanAt.Equal(lastScan) {
		t.Fatalf("LastScanAt = %v, want it untouched at %v", f.LastScanAt, lastScan)
	}
	if f.LastError != "" {
		t.Fatalf("LastError = %q, want the loop to leave it to the scanner", f.LastError)
	}
}

// testPeer returns a peer record pointing at the fake dialer's address.
func testPeer(id device.ID, addr string) device.Device {
	return device.Device{ID: id, Name: "nas", Address: addr, Verified: true, AddedAt: time.Unix(0, 0).UTC()}
}

func TestDaemon_ConnectedPeerAppearsInStatus(t *testing.T) {
	const peerID = device.ID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	// A short rescan interval makes the daemon re-read the peer table.
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: 50 * time.Millisecond}, nil)
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
	})

	// The peer table is re-read on reload, like the folder table.
	if err := h.store.SaveDevice(h.ctx, testPeer(peerID, "nas:22000")); err != nil {
		t.Fatalf("save device: %v", err)
	}

	waitFor(t, "peer connection", func() bool {
		report, err := h.daemon.Status(h.ctx)
		if err != nil {
			return false
		}
		return len(report.Peers) == 1 && report.Peers[0].Connected
	})

	if dialed := h.dialer.Dialed(); len(dialed) == 0 || dialed[0] != "nas:22000" {
		t.Fatalf("dialed = %v, want the peer address", dialed)
	}
}

func TestDaemon_UnreachablePeerReportsError(t *testing.T) {
	const peerID = device.ID("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	h := startDaemonWithPeers(t, daemon.Config{}, []device.Device{testPeer(peerID, "ghost:22000")})

	waitFor(t, "peer failure", func() bool {
		report, err := h.daemon.Status(h.ctx)
		if err != nil || len(report.Peers) != 1 {
			return false
		}
		return !report.Peers[0].Connected && report.Peers[0].Error != nil
	})
}

// rootWatcher is a ports.Watcher whose root can be taken away: while the root
// is missing, Watch fails at once the way the fsnotify adapter does.
type rootWatcher struct {
	mu       sync.Mutex
	missing  bool
	attached int
}

// Watch blocks until ctx is done, or fails when the root is missing.
func (w *rootWatcher) Watch(ctx context.Context, _ string, _ chan<- ports.WatchEvent) error {
	w.mu.Lock()
	missing := w.missing
	if !missing {
		w.attached++
	}
	w.mu.Unlock()
	if missing {
		return errors.New("watch root is not a directory")
	}
	<-ctx.Done()
	return ctx.Err()
}

// setMissing takes the root away or puts it back.
func (w *rootWatcher) setMissing(missing bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.missing = missing
}

// attaches returns how often the watch was established.
func (w *rootWatcher) attaches() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attached
}

func TestDaemon_ReattachesWatchWhenTheRootComesBack(t *testing.T) {
	watcher := &rootWatcher{}
	h := startDaemonWatching(t, daemon.Config{RescanInterval: 20 * time.Millisecond},
		watcher, nil, testFolder("docs", false))
	waitFor(t, "startup watch", func() bool { return watcher.attaches() == 1 })

	// The root goes away: scans fail and the watch is dropped.
	watcher.setMissing(true)
	h.scanner.failWith(errors.New("folder root does not exist or is not a directory"))
	before := len(h.scanner.snapshot())
	waitFor(t, "a failing scan", func() bool { return len(h.scanner.snapshot()) > before })
	if got := watcher.attaches(); got != 1 {
		t.Fatalf("watch attaches = %d while the root is missing, want 1", got)
	}

	// The root comes back: the next tick watches it again and scans it.
	watcher.setMissing(false)
	h.scanner.failWith(nil)
	waitFor(t, "the watch to be re-established", func() bool { return watcher.attaches() >= 2 })
	after := len(h.scanner.snapshot())
	waitFor(t, "a scan after the root came back", func() bool { return len(h.scanner.snapshot()) > after })
}

// reload asks the daemon to re-read its configuration the way the CLI does,
// over the control API.
func (h *harness) reload(t *testing.T) {
	t.Helper()
	if resp := h.daemon.Handle(h.ctx, ports.ControlRequest{Cmd: daemon.CmdReload}); !resp.OK {
		t.Fatalf("reload = %+v, want ok", resp)
	}
}

func TestReload_WatchesAFolderAddedWhileRunning(t *testing.T) {
	// An hour of rescan interval: nothing but the reload can pick the new
	// folder up.
	h := startDaemon(t, daemon.Config{RescanInterval: time.Hour}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	if err := h.store.SaveFolder(h.ctx, testFolder("pics", false)); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	h.reload(t)

	waitFor(t, "the new folder to be scanned", func() bool {
		for _, call := range h.scanner.snapshot() {
			if call.ID == "pics" {
				return true
			}
		}
		return false
	})
	waitFor(t, "the watch on the new folder", func() bool {
		for _, root := range h.watcher.Roots() {
			if root == "/srv/pics" {
				return true
			}
		}
		return false
	})
}

func TestReload_StopsWatchingAFolderRemovedWhileRunning(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: time.Hour}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	if err := h.store.DeleteFolder(h.ctx, "docs"); err != nil {
		t.Fatalf("delete folder: %v", err)
	}
	h.reload(t)

	report, err := h.daemon.Status(h.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(report.Folders) != 0 {
		t.Fatalf("status lists %+v, want no folder", report.Folders)
	}
}

func TestReload_DialsAPeerAddedWhileRunning(t *testing.T) {
	const peerID = device.ID("CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
	// Without a reload the peer would only be dialled at the next rescan
	// tick, an hour away.
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: time.Hour}, nil, testFolder("docs", false))
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
	})

	if err := h.store.SaveDevice(h.ctx, testPeer(peerID, "nas:22000")); err != nil {
		t.Fatalf("save device: %v", err)
	}
	h.reload(t)

	waitFor(t, "the new peer to be dialled", func() bool {
		dialed := h.dialer.Dialed()
		return len(dialed) == 1 && dialed[0] == "nas:22000"
	})
}

func TestReload_AnnouncesTheFoldersToConnectedPeers(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: time.Hour}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	if err := h.store.SaveFolder(h.ctx, testFolder("pics", false)); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	h.reload(t)

	// The peers that are connected learn the new list without
	// reconnecting, so they can pull the folder both sides now have.
	announcements := h.listener.Announcements()
	if len(announcements) != 1 {
		t.Fatalf("%d announcements, want one per reload", len(announcements))
	}
	ids := make(map[folder.ID]folder.Mode)
	for _, advert := range announcements[0] {
		ids[advert.ID] = advert.Mode
	}
	if len(ids) != 2 || ids["docs"] != folder.ModeSendReceive || ids["pics"] != folder.ModeSendReceive {
		t.Fatalf("announced %+v, want docs and pics as sendreceive", ids)
	}
}

func TestReload_AnnouncesAModeChange(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: time.Hour}, testFolder("docs", false))

	changed := testFolder("docs", false)
	changed.Mode = folder.ModeSendOnly
	if err := h.store.SaveFolder(h.ctx, changed); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	h.reload(t)

	announcements := h.listener.Announcements()
	if len(announcements) != 1 || len(announcements[0]) != 1 {
		t.Fatalf("announcements = %+v, want one folder announced once", announcements)
	}
	if got := announcements[0][0]; got.ID != "docs" || got.Mode != folder.ModeSendOnly {
		t.Fatalf("announced %+v, want docs as sendonly", got)
	}
}

func TestPeerLoop_SyncsAFolderTheHelloDidNotList(t *testing.T) {
	const peerID = device.ID("DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD")
	// The peer connects announcing no folder at all, which is what a peer
	// that had not configured the folder yet does.
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: time.Hour, SyncInterval: 20 * time.Millisecond},
		[]device.Device{testPeer(peerID, "nas:22000")}, testFolder("docs", false))
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
	})

	var conn *fakes.PeerConn
	waitFor(t, "the peer connection", func() bool {
		conns := h.dialer.Conns()
		if len(conns) == 0 {
			return false
		}
		conn = conns[0]
		return true
	})
	if got := h.syncer.syncedFolders(); len(got) != 0 {
		t.Fatalf("synced %v, want nothing while the peer announces no folder", got)
	}

	// The peer adds the folder and says so on the live connection.
	conn.SetFolders([]ports.FolderAdvert{{ID: "docs", Mode: folder.ModeSendReceive}})

	waitFor(t, "the folder both sides now share to be synced", func() bool {
		for _, id := range h.syncer.syncedFolders() {
			if id == "docs" {
				return true
			}
		}
		return false
	})
}

// recordingControl is a ports.ControlClient that records the request and
// answers with a fixed response.
type recordingControl struct {
	req  ports.ControlRequest
	resp ports.ControlResponse
	err  error
}

// Call records the request and returns the scripted answer.
func (c *recordingControl) Call(_ context.Context, req ports.ControlRequest) (ports.ControlResponse, error) {
	c.req = req
	return c.resp, c.err
}

func TestClient_ReloadSendsTheReloadCommand(t *testing.T) {
	rpc := &recordingControl{resp: ports.ControlResponse{OK: true}}

	if err := daemon.NewClient(rpc).Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rpc.req.Cmd != daemon.CmdReload || rpc.req.FolderID != "" {
		t.Fatalf("request = %+v, want the reload command for every folder", rpc.req)
	}
}

func TestClient_ReloadWithoutADaemon(t *testing.T) {
	rpc := &recordingControl{err: ports.ErrDaemonUnavailable}

	// The CLI tells the difference between "no daemon" and a refusal, so
	// the sentinel has to survive the call.
	if err := daemon.NewClient(rpc).Reload(context.Background()); !errors.Is(err, ports.ErrDaemonUnavailable) {
		t.Fatalf("reload without a daemon = %v, want ErrDaemonUnavailable", err)
	}
}

func TestPeerLoop_ClosedConnectionIsReportedWithoutWaitingForATick(t *testing.T) {
	const peerID = device.ID("EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE")
	// Both the ping and the sync tick are an hour away, so only the read
	// loop ending can tell the daemon that the peer is gone.
	h := startDaemonWithPeers(t, daemon.Config{
		RescanInterval: time.Hour, SyncInterval: time.Hour, PeerPingInterval: time.Hour,
	}, []device.Device{testPeer(peerID, "nas:22000")}, testFolder("docs", false))
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
	})

	var conn *fakes.PeerConn
	waitFor(t, "the peer connection", func() bool {
		conns := h.dialer.Conns()
		if len(conns) == 0 {
			return false
		}
		conn = conns[0]
		return peerConnected(t, h)
	})

	// The peer goes away: its connection ends without anything being asked
	// of it, and it is not there to be dialled again.
	h.dialer.Set("nas:22000", fakes.PeerScript{Err: &ports.ConnectError{
		Stage: ports.StageDial, Err: errors.New("connection refused"),
	}})
	if err := conn.Close(); err != nil {
		t.Fatalf("close the peer connection: %v", err)
	}

	waitFor(t, "the peer to be reported as disconnected", func() bool { return !peerConnected(t, h) })
}

// peerConnected reports whether status says the only peer is connected.
func peerConnected(t *testing.T, h *harness) bool {
	t.Helper()
	report, err := h.daemon.Status(h.ctx)
	if err != nil || len(report.Peers) != 1 {
		return false
	}
	return report.Peers[0].Connected
}

func TestReload_SyncsEveryConnectedPeerAtOnce(t *testing.T) {
	const peerID = device.ID("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")
	// An hour of sync interval: only the reload can make the next cycle
	// happen, which is what a local `folder set` needs.
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: time.Hour, SyncInterval: time.Hour},
		[]device.Device{testPeer(peerID, "nas:22000")}, testFolder("docs", false))
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe:   ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
		Handler: folderAnnouncer{store: h.store},
	})

	// The cycle that runs on connect is the one to wait for; the reload
	// has to add another.
	waitFor(t, "the sync cycle that follows the connection", func() bool {
		return len(h.syncer.syncedFolders()) == 1
	})

	h.reload(t)

	waitFor(t, "a sync cycle triggered by the reload", func() bool {
		return len(h.syncer.syncedFolders()) >= 2
	})
}

// TestReload_WatchesTheNewPathOfAMovedFolder proves a folder whose path
// changed is watched where it is now. The loop holds the configuration it
// started with, so one that keeps running after a move watches a directory the
// folder no longer has.
func TestReload_WatchesTheNewPathOfAMovedFolder(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: time.Hour}, testFolder("docs", false))
	waitFor(t, "the watch on the original path", func() bool {
		for _, root := range h.watcher.Roots() {
			if root == "/srv/docs" {
				return true
			}
		}
		return false
	})

	moved := testFolder("docs", false)
	moved.Path = "/srv/elsewhere"
	if err := h.store.SaveFolder(h.ctx, moved); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	h.reload(t)

	waitFor(t, "the watch on the new path", func() bool {
		for _, root := range h.watcher.Roots() {
			if root == "/srv/elsewhere" {
				return true
			}
		}
		return false
	})
}

// TestReload_KeepsTheLoopOfAnUnchangedFolder proves a reload does not restart
// what it does not have to: a restart costs a full rescan of the folder.
func TestReload_KeepsTheLoopOfAnUnchangedFolder(t *testing.T) {
	h := startDaemon(t, daemon.Config{RescanInterval: time.Hour}, testFolder("docs", false))
	waitFor(t, "startup scan", func() bool { return len(h.scanner.snapshot()) == 1 })

	// The same folder with another label: nothing the loop reads at start.
	relabelled := testFolder("docs", false)
	relabelled.Label = "My documents"
	if err := h.store.SaveFolder(h.ctx, relabelled); err != nil {
		t.Fatalf("save folder: %v", err)
	}
	h.reload(t)

	if got := len(h.scanner.snapshot()); got != 1 {
		t.Fatalf("%d scans, want the loop left alone", got)
	}
	if got := len(h.watcher.Roots()); got != 1 {
		t.Fatalf("%d watches, want the loop left alone", got)
	}
}

// TestReload_PeerAddressChangeKeepsTheNewConnection proves the loop for a
// peer's new address is not left stateless by the old loop stopping: the old
// one only forgets what it reported if no other loop has taken the peer over.
func TestReload_PeerAddressChangeKeepsTheNewConnection(t *testing.T) {
	const peerID = device.ID("DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD")
	// An hour of everything: only the reload moves the peer, and only the
	// new loop's first ping reports it, so a state wiped afterwards stays
	// wiped.
	cfg := daemon.Config{RescanInterval: time.Hour, PeerPingInterval: time.Hour, SyncInterval: time.Hour}
	h := startDaemonWithPeers(t, cfg, []device.Device{testPeer(peerID, "nas:22000")})
	for _, addr := range []string{"nas:22000", "nas:22001"} {
		h.dialer.Set(addr, fakes.PeerScript{
			Probe: ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
		})
	}
	waitFor(t, "the first connection", func() bool {
		report, err := h.daemon.Status(h.ctx)
		return err == nil && len(report.Peers) == 1 && report.Peers[0].Connected
	})

	if err := h.store.SaveDevice(h.ctx, testPeer(peerID, "nas:22001")); err != nil {
		t.Fatalf("save device: %v", err)
	}
	h.reload(t)

	waitFor(t, "the peer to be dialled at its new address", func() bool {
		for _, addr := range h.dialer.Dialed() {
			if addr == "nas:22001" {
				return true
			}
		}
		return false
	})
	// The state the new loop reported must survive the old loop stopping.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		report, err := h.daemon.Status(h.ctx)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if len(report.Peers) != 1 || !report.Peers[0].Connected {
			t.Fatalf("peer = %+v, want it reported as connected at its new address", report.Peers)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// limited wires a daemon with a settings store and a limiter per direction,
// and returns all three.
func limited(t *testing.T, settings *fakes.Settings) (*harness, *fakes.Limiter, *fakes.Limiter) {
	t.Helper()
	up, down := fakes.NewLimiter(), fakes.NewLimiter()
	h := startDaemonTweaked(t, daemon.Config{}, nil, func(d *daemon.Deps) {
		d.Settings = settings
		d.Upload = up
		d.Download = down
	}, nil, folder.Folder{ID: "docs", Path: folderRoot, Mode: folder.ModeSendReceive})
	return h, up, down
}

func TestRun_AppliesBandwidthLimitsFromSettings(t *testing.T) {
	settings := fakes.NewSettings(ports.Settings{UploadLimit: 4 << 20, DownloadLimit: 1 << 20})
	_, up, down := limited(t, settings)

	if got := up.Rate(); got != bandwidth.Rate(4<<20) {
		t.Fatalf("upload rate = %v, want 4 MiB/s", got)
	}
	if got := down.Rate(); got != bandwidth.Rate(1<<20) {
		t.Fatalf("download rate = %v, want 1 MiB/s", got)
	}
}

// TestReload_AppliesChangedBandwidthLimits is what `config set` relies on: the
// rate written to the settings store reaches the running limiters without any
// connection being dropped.
func TestReload_AppliesChangedBandwidthLimits(t *testing.T) {
	settings := fakes.NewSettings(ports.Settings{UploadLimit: 4 << 20, DownloadLimit: 4 << 20})
	h, up, down := limited(t, settings)

	settings.Queue(ports.Settings{UploadLimit: 1 << 20, DownloadLimit: bandwidth.Unlimited})
	if err := h.daemon.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := up.Rate(); got != bandwidth.Rate(1<<20) {
		t.Fatalf("upload rate = %v, want 1 MiB/s after the reload", got)
	}
	if got := down.Rate(); !got.Unlimited() {
		t.Fatalf("download rate = %v, want unlimited after the reload", got)
	}
	if rates := up.Rates(); len(rates) != 2 {
		t.Fatalf("upload SetRate calls = %v, want one on start and one on reload", rates)
	}
}

func TestStatus_ReportsLimits(t *testing.T) {
	settings := fakes.NewSettings(ports.Settings{UploadLimit: 2 << 20, DownloadLimit: 0})
	h, _, _ := limited(t, settings)

	report, err := h.daemon.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if report.Daemon.UploadLimit != 2<<20 {
		t.Fatalf("daemon.upload_limit = %d, want 2097152", report.Daemon.UploadLimit)
	}
	if report.Daemon.DownloadLimit != 0 {
		t.Fatalf("daemon.download_limit = %d, want 0 for unlimited", report.Daemon.DownloadLimit)
	}
}

// closedFolder returns a folder whose sync window excludes the fake clock's
// time, which reads 10:00 UTC.
func closedFolder(t *testing.T, id folder.ID) folder.Folder {
	t.Helper()
	f := testFolder(id, false)
	windows, err := folder.ParseWindows([]string{"22:00-06:00"})
	if err != nil {
		t.Fatalf("parse window: %v", err)
	}
	f.SyncWindows = windows
	return f
}

func TestPeerLoop_SkipsFolderOutsideItsWindow(t *testing.T) {
	const peerID = device.ID("WWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWW")
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: time.Hour, SyncInterval: 20 * time.Millisecond},
		[]device.Device{testPeer(peerID, "nas:22000")},
		testFolder("open", false), closedFolder(t, "closed"))
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe:   ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
		Handler: folderAnnouncer{store: h.store},
	})

	// Several cycles run; the closed folder is in none of them.
	waitFor(t, "two sync cycles", func() bool { return len(h.syncer.syncedFolders()) >= 2 })
	for _, id := range h.syncer.syncedFolders() {
		if id != "open" {
			t.Fatalf("synced %v, want only the folder inside its window", h.syncer.syncedFolders())
		}
	}
}

func TestReload_WindowRemovedSyncsAtOnce(t *testing.T) {
	const peerID = device.ID("XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")
	// An hour of sync interval: only the reload `folder set` asks for can
	// make the folder sync now that its window is gone.
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: time.Hour, SyncInterval: time.Hour},
		[]device.Device{testPeer(peerID, "nas:22000")}, closedFolder(t, "docs"))
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe:   ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
		Handler: folderAnnouncer{store: h.store},
	})
	waitFor(t, "the peer connection", func() bool { return len(h.dialer.Conns()) == 1 })
	if got := h.syncer.syncedFolders(); len(got) != 0 {
		t.Fatalf("synced %v, want nothing while the folder is outside its window", got)
	}

	open := testFolder("docs", false)
	if err := h.store.SaveFolder(h.ctx, open); err != nil {
		t.Fatalf("clear the sync windows: %v", err)
	}
	h.reload(t)

	waitFor(t, "a sync cycle once the window is gone", func() bool {
		return len(h.syncer.syncedFolders()) >= 1
	})
}

func TestResume_SyncsAtOnce(t *testing.T) {
	const peerID = device.ID("XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")
	// An hour of sync interval: only the resume can make the folder sync
	// before the test gives up.
	h := startDaemonWithPeers(t, daemon.Config{RescanInterval: time.Hour, SyncInterval: time.Hour},
		[]device.Device{testPeer(peerID, "nas:22000")}, testFolder("docs", true))
	// The remote side advertises docs whatever this device does with it:
	// the pause under test is local.
	h.dialer.Set("nas:22000", fakes.PeerScript{
		Probe:   ports.ProbeResult{DeviceID: peerID, Name: "nas", Accepted: true},
		Handler: staticAnnouncer{folders: []folder.ID{"docs"}},
	})
	waitFor(t, "the peer connection", func() bool { return len(h.dialer.Conns()) == 1 })
	if got := h.syncer.syncedFolders(); len(got) != 0 {
		t.Fatalf("synced %v, want nothing while the folder is paused", got)
	}

	if resp := h.daemon.Handle(h.ctx, ports.ControlRequest{Cmd: daemon.CmdResume, FolderID: "docs"}); !resp.OK {
		t.Fatalf("resume = %+v, want ok", resp)
	}

	waitFor(t, "a sync cycle right after the resume", func() bool {
		return len(h.syncer.syncedFolders()) >= 1
	})
}

// staticAnnouncer is a ports.PeerHandler that advertises a fixed folder list,
// so what the local device does with its own folders does not change what the
// peer says it serves.
type staticAnnouncer struct {
	folders []folder.ID
}

// Hello announces the fixed folder list.
func (h staticAnnouncer) Hello(context.Context) ports.HelloInfo {
	info := ports.HelloInfo{DeviceID: "DEVICE", Name: "test", Folders: []ports.FolderAdvert{}}
	for _, id := range h.folders {
		info.Folders = append(info.Folders, ports.FolderAdvert{ID: id, Mode: folder.ModeSendReceive})
	}
	return info
}

// Manifest serves nothing.
func (staticAnnouncer) Manifest(context.Context, device.ID, folder.ID) (manifest.Manifest, error) {
	return manifest.Manifest{}, errors.New("this fake peer handler serves no manifests")
}

// Chunk serves nothing.
func (staticAnnouncer) Chunk(context.Context, device.ID, ports.ChunkRequest) (ports.ChunkData, error) {
	return ports.ChunkData{}, errors.New("this fake peer handler serves no chunks")
}
