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

// crashMutatedDirs is how much of the tree an attempt rewrites before it
// scans: 20 directories of 100 files each, so the scan has 2,000 changes to
// write in one transaction and a kill has something to interrupt.
const crashMutatedDirs = 20

// crashFixture is the file tree the crash tests scan, built once and rewritten
// in place by each attempt. The revision is what the current contents were
// written at, and the byte count follows it, so the tests share one tree and
// still know what a complete scan of it must record.
var (
	crashFixtureOnce     sync.Once
	crashFixtureRoot     string
	crashFixtureBytes    int64
	crashFixtureRevision int
	crashFixtureErr      error
)

// requireBinary skips the test when it cannot have a child process to kill.
func requireBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary and kills it mid-scan; run without -short")
	}
	path, err := binary()
	if err != nil {
		t.Skipf("no syncthis binary to kill: %v", err)
	}
	return path
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
			if err := writeFixtureFile(root, d, f, 0); err != nil {
				return 0, err
			}
			total += int64(fixtureSize(d, f, 0))
		}
	}
	return total, nil
}

// fixtureSize is the size a file has at a revision. Consecutive revisions of
// the same file always differ in size, so a scan cannot miss the change
// whatever the file system says about modification times.
func fixtureSize(dir, file, revision int) int {
	return 32 + (dir*crashFilesEach+file+revision*37)%225
}

// writeFixtureFile writes one file of the tree at the given revision.
func writeFixtureFile(root string, dir, file, revision int) error {
	size := fixtureSize(dir, file, revision)
	content := bytes.Repeat([]byte{byte('a' + (dir+file+revision)%26)}, size)
	path := filepath.Join(root, fmt.Sprintf("d%03d", dir), fmt.Sprintf("f%03d.txt", file))
	return os.WriteFile(path, content, 0o644)
}

// mutateFixture rewrites the first crashMutatedDirs directories at the next
// revision and returns the root and the new total size of the tree. It is what
// gives the next scan a large write to be interrupted in: a scan of a tree
// that was already recorded writes nothing, and killing it proves nothing.
func mutateFixture(t *testing.T) (string, int64) {
	t.Helper()
	root, total := fixtureTree(t)
	revision := crashFixtureRevision + 1
	for d := range crashMutatedDirs {
		for f := range crashFilesEach {
			if err := writeFixtureFile(root, d, f, revision); err != nil {
				t.Fatalf("rewrite the fixture tree: %v", err)
			}
			total += int64(fixtureSize(d, f, revision) - fixtureSize(d, f, revision-1))
		}
	}
	crashFixtureRevision = revision
	crashFixtureBytes = total
	return root, total
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
	path := requireBinary(t)
	c := &child{done: make(chan struct{})}
	c.cmd = exec.Command(path, append([]string{"--data-dir", dataDir}, args...)...)
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
	path := requireBinary(t)
	cmd := exec.Command(path, append([]string{"--data-dir", dataDir}, args...)...)
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

// folderState is what the store holds for the scanned folder.
type folderState struct {
	Files int64
	Bytes int64
}

// stateOf reads the recorded file count and total size of the folder.
func stateOf(t *testing.T, dataDir string) folderState {
	t.Helper()
	report := statusOf(t, dataDir)
	if len(report.Folders) != 1 {
		t.Fatalf("folders = %+v, want one", report.Folders)
	}
	return folderState{Files: report.Folders[0].Files, Bytes: report.Folders[0].Bytes}
}

// assertRecovers runs the checks every kill in these tests is followed by: the
// database opens, repair finds nothing it cannot fix, and a rescan converges
// on the whole tree without finding anything left to do afterwards. What the
// store held right after the kill is checked by the caller, which knows what
// was there before.
func assertRecovers(t *testing.T, dataDir string, total int64) {
	t.Helper()

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

	if got, want := stateOf(t, dataDir), (folderState{Files: crashFiles, Bytes: total}); got != want {
		t.Fatalf("state after the rescan = %+v, want %+v", got, want)
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

// The phases of a scan a kill can land in. They are read from the debug log
// the child writes: the scanner logs "scan writing" when the walk is over and
// the whole result is about to be written in one transaction, and "scan
// completed" once it has been.
const (
	phaseWalk      = "walk"
	phaseWrite     = "write"
	phaseCompleted = "completed"
)

// phaseOf reports which phase the child was in when it stopped.
func phaseOf(log string) string {
	switch {
	case strings.Contains(log, "scan completed"):
		return phaseCompleted
	case strings.Contains(log, "scan writing"):
		return phaseWrite
	default:
		return phaseWalk
	}
}

// scanTiming is how long a scan of the fixture spends reading the tree and how
// long it spends writing what it found, on the machine running the test.
type scanTiming struct {
	walk  time.Duration
	write time.Duration
}

// logTime returns the time of the first debug line with the given message.
func logTime(t *testing.T, log, message string) time.Time {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		var record struct {
			Time time.Time `json:"time"`
			Msg  string    `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record.Msg == message {
			return record.Time
		}
	}
	t.Fatalf("the child never logged %q:\n%s", message, truncate(log))
	return time.Time{}
}

// crashRun is one data directory, the tree it scans and what a complete scan
// of that tree must record.
type crashRun struct {
	dataDir string
	root    string
	total   int64
}

// newCrashRun prepares the data directory, the folder and a first complete
// scan, so what follows is measured and killed under the conditions of an
// attempt: a store that already holds the tree and a scan that has 2,000
// changed files to write.
func newCrashRun(t *testing.T) *crashRun {
	t.Helper()
	dataDir, _ := crashEnv(t)
	root, total := mutateFixture(t)
	if code, out := runBinary(t, dataDir, "scan", "docs", "--json"); code != 0 {
		t.Fatalf("the first scan exit = %d, want 0 (%s)", code, truncate(out))
	}
	return &crashRun{dataDir: dataDir, root: root, total: total}
}

// measure rewrites part of the tree and scans it to completion, and returns how
// long each phase of that scan took. The kill delays are derived from this
// rather than guessed, so they land where they are meant to on a fast machine
// and on a slow one.
func (r *crashRun) measure(t *testing.T) scanTiming {
	t.Helper()
	root, total := mutateFixture(t)
	r.root, r.total = root, total
	started := time.Now()
	c := startChild(t, r.dataDir, "scan", "docs", "--json",
		"--log-level", "debug", "--log-format", "json")
	if err := c.wait(t, 5*time.Minute*timeoutScale); err != nil {
		t.Fatalf("the measuring scan failed: %v (stderr: %s)", err, c.errorOutput())
	}
	log := c.stderr.String()
	writing := logTime(t, log, "scan writing")
	completed := logTime(t, log, "scan completed")
	if got, want := stateOf(t, r.dataDir), (folderState{Files: crashFiles, Bytes: total}); got != want {
		t.Fatalf("state after the measuring scan = %+v, want %+v", got, want)
	}
	return scanTiming{walk: writing.Sub(started), write: completed.Sub(writing)}
}

// attempt rewrites part of the tree, scans it, kills the scan after delay and
// checks what the store holds afterwards: either exactly what it held before
// the attempt or exactly what the completed scan would have recorded, never
// anything in between. It returns the phase the kill landed in.
func (r *crashRun) attempt(t *testing.T, delay time.Duration) string {
	t.Helper()
	root, total := mutateFixture(t)
	r.root, r.total = root, total
	before := stateOf(t, r.dataDir)
	after := folderState{Files: crashFiles, Bytes: total}

	c := startChild(t, r.dataDir, "scan", "docs", "--json",
		"--log-level", "debug", "--log-format", "json")
	killed := c.killAfter(t, delay)
	phase := phaseOf(c.errorOutput())
	t.Logf("kill after %s: killed = %t, phase = %s", delay, killed, phase)

	got := stateOf(t, r.dataDir)
	switch phase {
	case phaseCompleted:
		if got != after {
			t.Fatalf("state = %+v after a scan that finished, want %+v", got, after)
		}
	case phaseWalk:
		// Nothing is written until the whole folder has been read, so
		// a kill during the walk must leave the store as it was.
		if got != before {
			t.Fatalf("state = %+v after a kill during the walk, want the untouched %+v",
				got, before)
		}
	default:
		if got != before && got != after {
			t.Fatalf("state = %+v after a kill during the write, want %+v or %+v: "+
				"the scan write is not atomic", got, before, after)
		}
	}
	assertRecovers(t, r.dataDir, total)
	return phase
}

// TestCrash_ScanKilledMidWayLeavesARecoverableDatabase kills a scan of 20,000
// files with SIGKILL and checks that the write it interrupted was all or
// nothing.
func TestCrash_ScanKilledMidWayLeavesARecoverableDatabase(t *testing.T) {
	requireBinary(t)
	run := newCrashRun(t)

	timing := run.measure(t)
	t.Logf("a scan of this fixture walks for %s and writes for %s", timing.walk, timing.write)

	// Three of the delays aim inside the write window, which is the short
	// phase and the one the claim is about; the others check the phases
	// around it.
	delays := []time.Duration{
		timing.walk / 2,
		timing.walk + timing.write/5,
		timing.walk + timing.write/2,
		timing.walk + (4*timing.write)/5,
		timing.walk + timing.write + timing.write/2,
	}

	attempts, inWrite := 0, 0
	for _, delay := range delays {
		attempts++
		if run.attempt(t, delay) == phaseWrite {
			inWrite++
		}
	}
	// The write window is tens of milliseconds wide, so a loaded machine
	// can miss it. Aiming at its middle again costs one scan each time and
	// keeps the claim from depending on how busy the machine was.
	for retry := 0; inWrite == 0 && retry < 4; retry++ {
		timing = run.measure(t)
		attempts++
		if run.attempt(t, timing.walk+timing.write/2) == phaseWrite {
			inWrite++
		}
	}
	if inWrite == 0 {
		t.Fatalf("no attempt of %d landed in the write phase; the atomicity claim was not exercised",
			attempts)
	}
	t.Logf("%d of %d attempts were killed during the write phase", inWrite, attempts)
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
