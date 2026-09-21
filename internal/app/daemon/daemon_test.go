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
	"github.com/wesbragagt/syncthis/internal/domain/folder"
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

// nopRepairer is a daemon.Repairer that reports nothing to fix.
type nopRepairer struct{}

// Run returns an empty report.
func (nopRepairer) Run(_ context.Context, id folder.ID, dryRun bool) (repair.Report, error) {
	return repair.Report{FolderID: id, DryRun: dryRun}, nil
}

// harness is a daemon running against fakes.
type harness struct {
	daemon  *daemon.Daemon
	store   *fakes.Store
	watcher *fakes.Watcher
	scanner *recordingScanner
	done    chan error
	cancel  context.CancelFunc
	ctx     context.Context

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

	store := fakes.NewStore()
	for _, f := range folders {
		if err := store.SaveFolder(context.Background(), f); err != nil {
			t.Fatalf("save folder: %v", err)
		}
	}
	clock := fakes.NewClock(time.Time{})
	identity := fakes.NewIdentity("DEVICE", "test")
	watcher := fakes.NewWatcher()
	scanner := newRecordingScanner()

	if cfg.RescanInterval == 0 {
		cfg.RescanInterval = time.Hour
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = 10 * time.Millisecond
	}
	d := daemon.New(cfg, daemon.Deps{
		Store:   store,
		Clock:   clock,
		Watcher: watcher,
		Scanner: scanner,
		Repair:  nopRepairer{},
		Status:  status.New(store, identity, clock, "/data"),
	})

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{daemon: d, store: store, watcher: watcher, scanner: scanner,
		done: make(chan error, 1), cancel: cancel, ctx: ctx, stopped: make(chan struct{})}
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
	if roots := h.watcher.Roots(); len(roots) != 1 || roots[0] != folderRoot {
		t.Fatalf("watched roots = %v, want [%s]", roots, folderRoot)
	}
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
