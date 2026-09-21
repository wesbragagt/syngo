package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The fixture the crash tests scan: 200 directories of 100 files each. It is
// large enough that a scan takes far longer than the first kill delays and
// small enough to build once per package run.
const (
	crashDirs      = 200
	crashFilesEach = 100
	crashFiles     = crashDirs * crashFilesEach
)

// crashKillDelays are how long each attempt lets the scan run before it kills
// it. They grow, so an attempt that lands after the scan finished on a fast
// machine is followed by ones that land earlier on a slow one; the test needs
// one kill in the middle of a scan, not six.
var crashKillDelays = []time.Duration{
	5 * time.Millisecond, 20 * time.Millisecond, 60 * time.Millisecond,
	150 * time.Millisecond, 400 * time.Millisecond, time.Second,
}

// crashFixture is the file tree the crash tests scan, built once.
var (
	crashFixtureOnce  sync.Once
	crashFixtureRoot  string
	crashFixtureBytes int64
	crashFixtureErr   error
)

// requireBinary skips the test when it cannot have a child process to kill.
func requireBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary and kills it mid-scan; run without -short")
	}
	if syncthisBinary == "" {
		t.Skipf("no syncthis binary to kill: %v", buildErr)
	}
	return syncthisBinary
}

// fixtureTree returns the shared file tree and the total size of its files.
func fixtureTree(t *testing.T) (string, int64) {
	t.Helper()
	crashFixtureOnce.Do(func() {
		root := filepath.Join(buildDir, "tree")
		crashFixtureBytes, crashFixtureErr = makeTree(root, crashDirs, crashFilesEach)
		crashFixtureRoot = root
	})
	if crashFixtureErr != nil {
		t.Fatalf("build the fixture tree: %v", crashFixtureErr)
	}
	return crashFixtureRoot, crashFixtureBytes
}

// makeTree writes dirs directories of files each, with deterministic names and
// contents of 32 to 256 bytes, and returns the total size.
func makeTree(root string, dirs, files int) (int64, error) {
	var total int64
	for d := range dirs {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
		for f := range files {
			size := 32 + (d*files+f)%225
			content := bytes.Repeat([]byte{byte('a' + (d+f)%26)}, size)
			path := filepath.Join(dir, fmt.Sprintf("f%03d.txt", f))
			if err := os.WriteFile(path, content, 0o644); err != nil {
				return 0, err
			}
			total += int64(size)
		}
	}
	return total, nil
}

// child is a syncthis process the test can kill. Everything the process
// writes, and its exit status, is only read once Wait has returned: os/exec
// fills both from goroutines of its own.
type child struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	// done is closed once Wait returned, after which err, state and the
	// output buffers are settled.
	done  chan struct{}
	err   error
	state *os.ProcessState
}

// startChild runs the binary against a data directory and returns as soon as
// the process is started.
func startChild(t *testing.T, dataDir string, args ...string) *child {
	t.Helper()
	binary := requireBinary(t)
	c := &child{done: make(chan struct{})}
	c.cmd = exec.Command(binary, append([]string{"--data-dir", dataDir}, args...)...)
	c.cmd.Stdout = &c.stdout
	c.cmd.Stderr = &c.stderr
	if err := c.cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	go func() {
		c.err = c.cmd.Wait()
		c.state = c.cmd.ProcessState
		close(c.done)
	}()
	t.Cleanup(func() {
		if c.finished() {
			return
		}
		_ = c.cmd.Process.Kill()
		<-c.done
	})
	return c
}

// finished reports whether the process has been waited for.
func (c *child) finished() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// signal sends a signal to the child and reports whether it was still running.
func (c *child) signal(sig syscall.Signal) bool {
	if c.finished() {
		return false
	}
	return c.cmd.Process.Signal(sig) == nil
}

// wait blocks until the child exits, or fails the test after the timeout.
func (c *child) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-c.done:
		return c.err
	case <-time.After(timeout):
		_ = c.cmd.Process.Kill()
		<-c.done
		t.Fatalf("the child did not exit within %s (stderr: %s)", timeout, c.errorOutput())
		return nil
	}
}

// errorOutput returns what the child wrote to standard error. It is only
// readable once the process has been waited for.
func (c *child) errorOutput() string {
	if !c.finished() {
		return "(the child is still running)"
	}
	return truncate(c.stderr.String())
}

// killAfter lets the child run for d, then sends SIGKILL if it is still there.
// It reports whether the process was killed rather than finishing on its own.
func (c *child) killAfter(t *testing.T, d time.Duration) bool {
	t.Helper()
	select {
	case <-c.done:
		return false
	case <-time.After(d):
	}
	if !c.signal(syscall.SIGKILL) {
		return false
	}
	_ = c.wait(t, 30*time.Second*timeoutScale)

	status, ok := c.state.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("process state = %v, want a wait status", c.state)
	}
	// The process may have finished between the timer firing and the
	// signal being delivered. That is a completed attempt, not a kill.
	if !status.Signaled() {
		return false
	}
	if status.Signal() != syscall.SIGKILL {
		t.Fatalf("process state = %v, want it killed by SIGKILL", c.state)
	}
	return true
}

// runBinary runs one command to completion and returns its exit code and
// standard output.
func runBinary(t *testing.T, dataDir string, args ...string) (int, string) {
	t.Helper()
	binary := requireBinary(t)
	cmd := exec.Command(binary, append([]string{"--data-dir", dataDir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errorsAs(err, &exitErr):
	default:
		t.Fatalf("run %v: %v (stderr: %s)", args, err, stderr.String())
	}
	return cmd.ProcessState.ExitCode(), stdout.String()
}

// errorsAs is errors.As, wrapped so the switch above reads as a list of cases.
func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// crashEnv is an initialized data directory with the fixture tree configured
// as the folder "docs".
func crashEnv(t *testing.T) (dataDir string, total int64) {
	t.Helper()
	root, total := fixtureTree(t)
	dataDir = filepath.Join(t.TempDir(), "data")
	if code, out := runBinary(t, dataDir, "init", "--name", "crash", "--json"); code != 0 {
		t.Fatalf("init exit = %d, want 0 (%s)", code, out)
	}
	if code, out := runBinary(t, dataDir, "folder", "add", root, "--id", "docs", "--json"); code != 0 {
		t.Fatalf("folder add exit = %d, want 0 (%s)", code, out)
	}
	return dataDir, total
}

// crashStatus is the part of `status --json` the crash tests read.
type crashStatus struct {
	Daemon struct {
		Running bool `json:"running"`
	} `json:"daemon"`
	Folders []struct {
		ID    string `json:"id"`
		Files int64  `json:"files"`
		Bytes int64  `json:"bytes"`
	} `json:"folders"`
}

// statusOf runs `status --json` and decodes it. A database left behind by a
// killed process that cannot be opened fails here, which is the first thing
// these tests claim.
func statusOf(t *testing.T, dataDir string) crashStatus {
	t.Helper()
	code, out := runBinary(t, dataDir, "status", "--json")
	if code != 0 {
		t.Fatalf("status exit = %d, want 0 (%s)", code, out)
	}
	var report crashStatus
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status output is not json: %v\n%s", err, out)
	}
	return report
}

// assertRecovers runs the checks every kill in these tests is followed by: the
// database opens and holds either the old manifest or the new one and nothing
// in between, repair finds nothing it cannot fix, and a rescan converges on
// the whole tree without finding anything left to do afterwards.
func assertRecovers(t *testing.T, dataDir string, total int64) {
	t.Helper()

	report := statusOf(t, dataDir)
	if len(report.Folders) != 1 {
		t.Fatalf("folders = %+v, want one", report.Folders)
	}
	// The scan writes its result in one transaction, so a kill leaves the
	// manifest it found or the one that was there, never a part of either.
	if files := report.Folders[0].Files; files != 0 && files != crashFiles {
		t.Fatalf("files = %d after the kill, want 0 or %d: the scan write is not atomic",
			files, crashFiles)
	}

	code, out := runBinary(t, dataDir, "repair", "docs", "--dry-run", "--json")
	if code != 0 && code != 10 {
		t.Fatalf("repair --dry-run exit = %d, want 0 or 10 (%s)", code, out)
	}
	var reports []struct {
		Unrecoverable []string `json:"unrecoverable"`
	}
	if err := json.Unmarshal([]byte(out), &reports); err != nil {
		t.Fatalf("repair output is not json: %v\n%s", err, out)
	}
	for _, r := range reports {
		if len(r.Unrecoverable) != 0 {
			t.Fatalf("unrecoverable = %v, want none", r.Unrecoverable)
		}
	}

	if code, out := runBinary(t, dataDir, "scan", "docs", "--json"); code != 0 {
		t.Fatalf("scan after the kill exit = %d, want 0 (%s)", code, truncate(out))
	}

	// A second scan finds nothing: the first one recorded everything, and
	// a repeated scan of an unchanged tree writes no further changes.
	code, out = runBinary(t, dataDir, "scan", "docs", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("scan --dry-run exit = %d, want 0 (%s)", code, truncate(out))
	}
	var scan struct {
		Summary struct {
			Changes int `json:"changes"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &scan); err != nil {
		t.Fatalf("scan output is not json: %v\n%s", err, truncate(out))
	}
	if scan.Summary.Changes != 0 {
		t.Fatalf("a repeated scan found %d changes, want none", scan.Summary.Changes)
	}

	report = statusOf(t, dataDir)
	if got := report.Folders[0].Files; got != crashFiles {
		t.Fatalf("files = %d after the rescan, want %d", got, crashFiles)
	}
	if got := report.Folders[0].Bytes; got != total {
		t.Fatalf("bytes = %d after the rescan, want %d", got, total)
	}
}

// truncate shortens command output so a failure message stays readable: a scan
// of 20,000 files prints one object per file.
func truncate(s string) string {
	if len(s) <= 2000 {
		return s
	}
	return s[:2000] + "… (truncated)"
}

// TestCrash_ScanKilledMidWayLeavesARecoverableDatabase kills a scan of 20,000
// files with SIGKILL and checks what the next process finds.
func TestCrash_ScanKilledMidWayLeavesARecoverableDatabase(t *testing.T) {
	requireBinary(t)
	dataDir, total := crashEnv(t)

	killedMidScan := 0
	for _, delay := range crashKillDelays {
		delay *= timeoutScale
		c := startChild(t, dataDir, "scan", "docs", "--json",
			"--log-level", "debug", "--log-format", "json")
		killed := c.killAfter(t, delay)
		if !killed {
			t.Logf("kill after %s: the scan had already finished", delay)
			assertRecovers(t, dataDir, total)
			continue
		}

		// The scanner logs "scan completed" once it has written its
		// result, so a process killed before that line was in the
		// middle of the walk or of the write.
		midScan := !strings.Contains(c.errorOutput(), "scan completed")
		t.Logf("kill after %s: SIGKILL landed on a running scan, mid-scan = %t", delay, midScan)
		if midScan {
			killedMidScan++
		}
		assertRecovers(t, dataDir, total)
	}

	if killedMidScan == 0 {
		t.Fatalf("no attempt killed the process while it was scanning %d files; "+
			"lower the delays in crashKillDelays", crashFiles)
	}
	t.Logf("%d of %d attempts killed the process mid-scan", killedMidScan, len(crashKillDelays))
}

// TestCrash_DaemonKilledDuringStartupScan kills a daemon while it scans at
// startup, then checks that the next daemon starts over the stale pid file and
// control socket and that the folder still converges.
func TestCrash_DaemonKilledDuringStartupScan(t *testing.T) {
	requireBinary(t)
	dataDir, total := crashEnv(t)

	first := startChild(t, dataDir, "daemon", "run", "--listen", "127.0.0.1:0",
		"--log-level", "debug", "--log-format", "json")
	pidFile := filepath.Join(dataDir, "daemon.pid")
	waitFor(t, "the pid file of the first daemon", daemonTimeout, func() bool {
		_, err := os.Stat(pidFile)
		return err == nil
	})
	if !first.killAfter(t, 100*time.Millisecond*timeoutScale) {
		t.Fatalf("the daemon exited on its own (stderr: %s)", first.errorOutput())
	}
	// A killed process cleans nothing up: this is what the next daemon has
	// to deal with.
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("the pid file is gone after SIGKILL: %v", err)
	}

	second := startChild(t, dataDir, "daemon", "run", "--listen", "127.0.0.1:0")
	waitFor(t, "the second daemon to take the data directory over", convergeTimeout, func() bool {
		return statusOf(t, dataDir).Daemon.Running
	})
	if !second.signal(syscall.SIGTERM) {
		t.Fatalf("the second daemon is not running any more (stderr: %s)", second.errorOutput())
	}
	if err := second.wait(t, daemonTimeout); err != nil {
		t.Fatalf("the second daemon exited with %v, want a clean shutdown (stderr: %s)",
			err, second.errorOutput())
	}

	assertRecovers(t, dataDir, total)
}
