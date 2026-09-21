package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
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
		Running   bool    `json:"running"`
		PID       int     `json:"pid"`
		Listen    string  `json:"listen"`
		StartedAt *string `json:"started_at"`
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
	waitForStatus(t, e, "the rename", func(r statusReport) bool { return r.Folders[0].Files == 2 })

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

	// A paused folder is not scanned, so the file stays invisible.
	write(t, e, "hidden.txt", "x")
	time.Sleep(700 * time.Millisecond)
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
