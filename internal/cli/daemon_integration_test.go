package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/cli"
	"github.com/wesbragagt/syncthis/internal/compose"
)

// convergeTimeout bounds how long a test waits for the daemon to pick a change up.
const convergeTimeout = 5 * time.Second

// statusReport is the part of `status --json` the daemon tests look at.
type statusReport struct {
	Daemon struct {
		Running       bool    `json:"running"`
		PID           int     `json:"pid"`
		Listen        string  `json:"listen"`
		StartedAt     *string `json:"started_at"`
		UploadLimit   int64   `json:"upload_limit"`
		DownloadLimit int64   `json:"download_limit"`
	} `json:"daemon"`
	Folders []struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Paused bool   `json:"paused"`
		Files  int64  `json:"files"`
		Dirs   int64  `json:"dirs"`
		Bytes  int64  `json:"bytes"`
	} `json:"folders"`
}

// daemonProcess is a `daemon run` invocation running inside the test process.
type daemonProcess struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	code   int
	stdout bytes.Buffer
	stderr bytes.Buffer
}

// stop cancels the daemon and returns its exit code.
func (p *daemonProcess) stop() int {
	p.cancel()
	p.wg.Wait()
	return p.code
}

// startDaemon runs `daemon run` in the background against the environment.
func startDaemon(t *testing.T, e env, args ...string) *daemonProcess {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &daemonProcess{cancel: cancel}
	full := append([]string{"--data-dir", e.dataDir, "daemon", "run"}, args...)
	// Tests must not fight over the configured port, so a daemon that was
	// not told where to listen gets one the operating system picks.
	if !slices.Contains(args, "--listen") {
		full = append(full, "--listen", "127.0.0.1:0")
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.code = cli.ExecuteContext(ctx, full, strings.NewReader(""), &p.stdout, &p.stderr)
	}()
	t.Cleanup(func() { p.stop() })
	return p
}

// status runs `status --json` and decodes it.
func status(t *testing.T, e env) statusReport {
	t.Helper()
	code, stdout, stderr := e.run("", "status", "--json")
	if code != cli.ExitOK {
		t.Fatalf("status exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("status output is not json: %v\n%s", err, stdout)
	}
	return report
}

// waitForStatus polls `status --json` until cond holds.
func waitForStatus(t *testing.T, e env, what string, cond func(statusReport) bool) statusReport {
	t.Helper()
	deadline := time.Now().Add(convergeTimeout)
	var last statusReport
	for time.Now().Before(deadline) {
		last = status(t, e)
		if cond(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last status: %+v", what, last)
	return last
}

// waitForDaemon waits until the daemon answers the control socket.
func waitForDaemon(t *testing.T, e env) {
	t.Helper()
	waitForStatus(t, e, "the daemon to answer", func(r statusReport) bool { return r.Daemon.Running })
}

// write writes a file inside the synchronized folder.
func write(t *testing.T, e env, name, content string) string {
	t.Helper()
	path := filepath.Join(e.folderPath, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestDaemonRun_DetectsFileWithinTwoSeconds(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)
	waitForStatus(t, e, "the initial scan", func(r statusReport) bool {
		return len(r.Folders) == 1 && r.Folders[0].State == "idle"
	})

	started := time.Now()
	write(t, e, "new.txt", "hello")

	report := waitForStatus(t, e, "the new file", func(r statusReport) bool {
		return len(r.Folders) == 1 && r.Folders[0].Files == 1
	})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("file detected after %s, want under 2s", elapsed)
	}
	if report.Folders[0].Bytes != int64(len("hello")) {
		t.Fatalf("bytes = %d, want 5", report.Folders[0].Bytes)
	}
}

func TestDaemonRun_TracksCreateModifyRenameDelete(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)

	write(t, e, "sub/a.txt", "one")
	write(t, e, "sub/b.txt", "two")
	waitForStatus(t, e, "two files", func(r statusReport) bool { return r.Folders[0].Files == 2 })

	write(t, e, "sub/a.txt", "one modified")
	waitForStatus(t, e, "the modified size", func(r statusReport) bool {
		return r.Folders[0].Bytes == int64(len("one modified")+len("two"))
	})

	if err := os.Rename(filepath.Join(e.folderPath, "sub/b.txt"), filepath.Join(e.folderPath, "sub/c.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// The file count does not change on a rename, so the stored names are
	// what proves the daemon saw it: a dry run finds nothing to change
	// only once the store holds sub/c.txt and no longer holds sub/b.txt.
	waitForClean(t, e, "the rename")

	if err := os.Remove(filepath.Join(e.folderPath, "sub/c.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	report := waitForStatus(t, e, "the deletion", func(r statusReport) bool { return r.Folders[0].Files == 1 })
	if report.Folders[0].State != "idle" {
		t.Fatalf("state = %q, want idle once the scan finished", report.Folders[0].State)
	}
}

func TestDaemonRun_StatusReportsTheDaemon(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	listen := closedPort(t)
	startDaemon(t, e, "--listen", listen)
	waitForDaemon(t, e)

	report := status(t, e)
	if report.Daemon.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", report.Daemon.PID, os.Getpid())
	}
	if report.Daemon.Listen != listen {
		t.Fatalf("listen = %q, want the --listen value", report.Daemon.Listen)
	}
	if report.Daemon.StartedAt == nil {
		t.Fatalf("started_at = null, want a timestamp")
	}
}

func TestDaemonRun_SecondInstanceExit6(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)

	code, _, stderr := e.run("", "daemon", "run")
	if code != cli.ExitDaemonUnavailable {
		t.Fatalf("second daemon exit = %d, want %d (stderr: %s)", code, cli.ExitDaemonUnavailable, stderr)
	}
	if !strings.Contains(stderr, "already running") {
		t.Fatalf("stderr = %q, want it to mention a running daemon", stderr)
	}
}

func TestDaemonRun_RemovesSocketAndPIDFileOnStop(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	p := startDaemon(t, e)
	waitForDaemon(t, e)
	for _, path := range []string{compose.ControlSocketPath(e.dataDir), compose.PIDFilePath(e.dataDir)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s missing while the daemon runs: %v", path, err)
		}
	}

	if code := p.stop(); code != cli.ExitOK {
		t.Fatalf("daemon exit = %d, want 0 (stderr: %s)", code, p.stderr.String())
	}
	for _, path := range []string{compose.ControlSocketPath(e.dataDir), compose.PIDFilePath(e.dataDir)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after shutdown (err %v)", path, err)
		}
	}
	if report := status(t, e); report.Daemon.Running {
		t.Fatalf("status still reports a running daemon after shutdown")
	}
}

func TestDaemonRun_StalePIDFileAndSocketAreReplaced(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)

	if err := os.WriteFile(compose.PIDFilePath(e.dataDir), []byte(deadPID(t)+"\n"), 0o600); err != nil {
		t.Fatalf("write stale pid file: %v", err)
	}
	if err := os.WriteFile(compose.ControlSocketPath(e.dataDir), nil, 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}

	startDaemon(t, e)
	waitForDaemon(t, e)
}

// TestDaemonRun_RestartsAfterAHardKillWhosePIDWasReused reproduces what an
// 8-hour soak hit after 924 restart rounds: a daemon killed with SIGKILL
// cannot clean up, so it leaves its pid file and its control socket behind,
// and inside a container its small pid is soon handed to an unrelated process.
// The restart must still claim the directory. The parent of the test runner
// stands in for the process that inherited the pid: alive, and not a daemon.
func TestDaemonRun_RestartsAfterAHardKillWhosePIDWasReused(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)

	reused := strconv.Itoa(os.Getppid())
	if err := os.WriteFile(compose.PIDFilePath(e.dataDir), []byte(reused+"\n"), 0o600); err != nil {
		t.Fatalf("write pid file naming a reused pid: %v", err)
	}
	ln, err := net.Listen("unix", compose.ControlSocketPath(e.dataDir))
	if err != nil {
		t.Fatalf("bind stale socket: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}

	startDaemon(t, e)
	waitForDaemon(t, e)
}

func TestDaemonRun_OrphanedUnixSocketIsReplaced(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)

	// A daemon killed without cleaning up leaves a real socket file that
	// nothing listens on any more.
	ln, err := net.Listen("unix", compose.ControlSocketPath(e.dataDir))
	if err != nil {
		t.Fatalf("bind stale socket: %v", err)
	}
	// Closing the listener must leave the socket file behind, the way a
	// killed daemon does.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}
	info, err := os.Stat(compose.ControlSocketPath(e.dataDir))
	if err != nil {
		t.Fatalf("stat stale socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stale path mode = %v, want a socket", info.Mode())
	}

	startDaemon(t, e)
	waitForDaemon(t, e)
}

func TestDaemonRun_ReWatchesARecreatedFolderRoot(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	// The reconciliation tick re-attaches the watch. It is long enough that
	// a file written right after the recovery can only be noticed by the
	// watch, and short enough for the test to wait for one tick.
	startDaemon(t, e, "--rescan-interval", "3s")
	waitForDaemon(t, e)
	write(t, e, "a.txt", "one")
	waitForStatus(t, e, "the first file", func(r statusReport) bool { return r.Folders[0].Files == 1 })

	// The root disappears: the folder goes into error and keeps its files.
	if err := os.RemoveAll(e.folderPath); err != nil {
		t.Fatalf("remove folder root: %v", err)
	}
	report := waitForStatus(t, e, "the folder to report the missing root", func(r statusReport) bool {
		return r.Folders[0].State == "error"
	})
	if report.Folders[0].Files != 1 {
		t.Fatalf("files = %d while the root is gone, want the stored entry kept", report.Folders[0].Files)
	}

	// The root comes back: the daemon watches it again, so a file written
	// after the reconciliation tick is picked up without a manual rescan.
	if err := os.MkdirAll(e.folderPath, 0o755); err != nil {
		t.Fatalf("recreate folder root: %v", err)
	}
	waitForStatus(t, e, "the folder to recover", func(r statusReport) bool {
		return r.Folders[0].State != "error" && r.Folders[0].Files == 0
	})

	// The recovery happened on a tick, so the next one is a rescan interval
	// away: a file seen before then went through the watch.
	started := time.Now()
	write(t, e, "b.txt", "two")
	waitForStatus(t, e, "the file written after the root came back", func(r statusReport) bool {
		return r.Folders[0].Files == 1
	})
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("file detected after %s, want the watch to report it well before the next rescan", elapsed)
	}
}

// waitForClean polls a dry-run scan until it finds nothing left to record,
// which means the store describes exactly what is on disk.
func waitForClean(t *testing.T, e env, what string) {
	t.Helper()
	deadline := time.Now().Add(convergeTimeout)
	var last scanOutput
	for time.Now().Before(deadline) {
		code, out := scanJSON(t, e, "--dry-run")
		last = out
		if code == cli.ExitOK && out.Summary.Changes == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; a dry run still reports %+v", what, last.Changes)
}

// deadPID returns the pid of a process that has already exited.
func deadPID(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run helper process: %v", err)
	}
	return strconv.Itoa(cmd.Process.Pid)
}

func TestPauseResume_ViaCLIReflectedInStatus(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)

	if code, stdout, stderr := e.run("", "pause", "docs"); code != cli.ExitOK {
		t.Fatalf("pause exit = %d (stdout %q, stderr %q)", code, stdout, stderr)
	}
	report := waitForStatus(t, e, "the paused folder", func(r statusReport) bool { return r.Folders[0].Paused })
	if report.Folders[0].State != "paused" {
		t.Fatalf("state = %q, want paused", report.Folders[0].State)
	}

	// A paused folder is not scanned, so the file stays invisible. Two
	// rescan requests are what gives the daemon its chance: each is a
	// round trip to the control socket, and the folder loop takes the
	// queued request and skips it because the folder is paused.
	write(t, e, "hidden.txt", "x")
	for range 2 {
		if code, _, stderr := e.run("", "rescan", "docs"); code != cli.ExitOK {
			t.Fatalf("rescan exit = %d (stderr %q)", code, stderr)
		}
	}
	if files := status(t, e).Folders[0].Files; files != 0 {
		t.Fatalf("files = %d while paused, want 0", files)
	}

	if code, _, stderr := e.run("", "resume", "docs"); code != cli.ExitOK {
		t.Fatalf("resume exit = %d (stderr %q)", code, stderr)
	}
	waitForStatus(t, e, "the folder to catch up", func(r statusReport) bool {
		return !r.Folders[0].Paused && r.Folders[0].Files == 1
	})
}

func TestPause_WithoutDaemonWritesTheDatabase(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)

	if code, _, stderr := e.run("", "pause", "docs"); code != cli.ExitOK {
		t.Fatalf("pause exit = %d (stderr %q)", code, stderr)
	}
	if !status(t, e).Folders[0].Paused {
		t.Fatalf("folder not paused after `pause` without a daemon")
	}
	if code, _, stderr := e.run("", "resume", "docs"); code != cli.ExitOK {
		t.Fatalf("resume exit = %d (stderr %q)", code, stderr)
	}
	if status(t, e).Folders[0].Paused {
		t.Fatalf("folder still paused after `resume` without a daemon")
	}
}

func TestPause_WithoutAFolderAppliesToAll(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	second := filepath.Join(filepath.Dir(e.folderPath), "photos")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", second, err)
	}
	if code, _, stderr := e.run("", "folder", "add", second, "--id", "photos"); code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	if code, stdout, stderr := e.run("", "pause"); code != cli.ExitOK {
		t.Fatalf("pause exit = %d (stdout %q, stderr %q)", code, stdout, stderr)
	}
	for _, f := range status(t, e).Folders {
		if !f.Paused || f.State != "paused" {
			t.Fatalf("folder %s = %+v, want it paused", f.ID, f)
		}
	}

	if code, _, stderr := e.run("", "resume"); code != cli.ExitOK {
		t.Fatalf("resume exit = %d (stderr %q)", code, stderr)
	}
	for _, f := range status(t, e).Folders {
		if f.Paused {
			t.Fatalf("folder %s is still paused", f.ID)
		}
	}
}

func TestPause_UnknownFolderIsNotFound(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)

	if code, _, _ := e.run("", "pause", "nope"); code != cli.ExitNotFound {
		t.Fatalf("pause exit = %d, want %d", code, cli.ExitNotFound)
	}
}

func TestRescan_WithDaemonIsQueued(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)
	write(t, e, "a.txt", "x")

	code, stdout, stderr := e.run("", "rescan", "docs", "--json")
	if code != cli.ExitOK {
		t.Fatalf("rescan exit = %d (stderr %q)", code, stderr)
	}
	var queued struct {
		Queued []string `json:"queued"`
	}
	if err := json.Unmarshal([]byte(stdout), &queued); err != nil {
		t.Fatalf("rescan output is not json: %v\n%s", err, stdout)
	}
	if len(queued.Queued) != 1 || queued.Queued[0] != "docs" {
		t.Fatalf("queued = %v, want [docs]", queued.Queued)
	}
	waitForStatus(t, e, "the queued rescan", func(r statusReport) bool { return r.Folders[0].Files == 1 })
}

func TestRepair_WithDaemonRunsInTheDaemon(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)
	write(t, e, "a.txt", "x")
	waitForStatus(t, e, "the new file", func(r statusReport) bool { return r.Folders[0].Files == 1 })

	code, stdout, stderr := e.run("", "repair", "docs", "--json")
	if code != cli.ExitOK {
		t.Fatalf("repair exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "\"folder_id\": \"docs\"") && !strings.Contains(stdout, "\"folder_id\":\"docs\"") {
		t.Fatalf("repair output = %q, want a report for docs", stdout)
	}
}

func TestPauseResume_WithoutAFolderWhileTheDaemonRuns(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	second := filepath.Join(filepath.Dir(e.folderPath), "photos")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", second, err)
	}
	if code, _, stderr := e.run("", "folder", "add", second, "--id", "photos"); code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	startDaemon(t, e)
	waitForDaemon(t, e)

	// No folder argument means every folder, and the running daemon is the
	// one that does it.
	if code, _, stderr := e.run("", "pause"); code != cli.ExitOK {
		t.Fatalf("pause exit = %d (stderr %q)", code, stderr)
	}
	waitForStatus(t, e, "both folders to be paused", func(r statusReport) bool {
		if len(r.Folders) != 2 {
			return false
		}
		return r.Folders[0].Paused && r.Folders[1].Paused
	})

	if code, _, stderr := e.run("", "resume"); code != cli.ExitOK {
		t.Fatalf("resume exit = %d (stderr %q)", code, stderr)
	}
	waitForStatus(t, e, "both folders to be resumed", func(r statusReport) bool {
		if len(r.Folders) != 2 {
			return false
		}
		return !r.Folders[0].Paused && !r.Folders[1].Paused
	})
}

func TestResume_AfterTheDaemonStopped(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	daemon := startDaemon(t, e)
	waitForDaemon(t, e)

	if code, _, stderr := e.run("", "pause", "docs"); code != cli.ExitOK {
		t.Fatalf("pause exit = %d (stderr %q)", code, stderr)
	}
	waitForStatus(t, e, "the paused folder", func(r statusReport) bool { return r.Folders[0].Paused })
	if code := daemon.stop(); code != cli.ExitOK {
		t.Fatalf("daemon exit = %d, want 0", code)
	}

	// With no daemon answering, the command writes the database itself
	// rather than failing.
	if code, _, stderr := e.run("", "resume"); code != cli.ExitOK {
		t.Fatalf("resume without a daemon exit = %d (stderr %q)", code, stderr)
	}
	report := status(t, e)
	if report.Daemon.Running {
		t.Fatal("the daemon is still answering after it was stopped")
	}
	if report.Folders[0].Paused {
		t.Fatal("the folder is still paused after `resume` with no daemon")
	}
}

func TestFolderAdd_WhileTheDaemonRunsIsPickedUpAtOnce(t *testing.T) {
	e := newEnv(t)
	setupFolder(t, e)
	startDaemon(t, e)
	waitForDaemon(t, e)

	second := filepath.Join(filepath.Dir(e.folderPath), "photos")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", second, err)
	}
	if err := os.WriteFile(filepath.Join(second, "holiday.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if code, _, stderr := e.run("", "folder", "add", second, "--id", "photos"); code != cli.ExitOK {
		t.Fatalf("folder add exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	// The rescan interval is ten minutes, so the folder being watched and
	// scanned within seconds is the reload the command asked for.
	waitForStatus(t, e, "the new folder to be scanned", func(r statusReport) bool {
		for _, f := range r.Folders {
			if f.ID == "photos" && f.Files == 1 {
				return true
			}
		}
		return false
	})
}
