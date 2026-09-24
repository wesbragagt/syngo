package e2e

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The reconnect test needs real operating system processes: the whole claim is
// that killing one of them leaves the others alone, and a daemon that lives in
// the test process cannot be told apart from the test. So this file builds the
// binary and runs everything through it, the way a person would.

// syngo runs one command with the binary and fails the test when it does
// not exit 0. It returns what the command printed on stdout.
func syngo(t *testing.T, bin, dataDir string, args ...string) string {
	t.Helper()
	full := append([]string{"--data-dir", dataDir}, args...)
	cmd := exec.Command(bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("syngo %s: %v\nstdout: %s\nstderr: %s",
			strings.Join(full, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// process is a syngo process the test started and can kill.
type process struct {
	t   *testing.T
	cmd *exec.Cmd
	// out collects everything the process wrote, which for the TUI is the
	// frames it drew.
	out *lockedBuffer
	// in is the process's standard input, which for the TUI is its
	// keyboard.
	in io.WriteCloser
	// waited guards Wait, which may only be called once.
	waited sync.Once
	status error
}

// lockedBuffer is a buffer a test reads while a process writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what has been written so far.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// start runs the binary as its own process, with its output collected.
func start(t *testing.T, bin string, stderr io.Writer, args ...string) *process {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = stderr
	// A pipe for stdin rather than nothing: the TUI reads keys from it,
	// and a closed stdin would look like the terminal going away.
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", strings.Join(args, " "), err)
	}
	p := &process{t: t, cmd: cmd, out: out, in: in}
	t.Cleanup(func() { p.kill(syscall.SIGKILL) })
	return p
}

// pid is the process id.
func (p *process) pid() int { return p.cmd.Process.Pid }

// alive reports whether the process still exists. Signal 0 asks the kernel
// that question without delivering anything.
func (p *process) alive() bool {
	if p.cmd.Process == nil {
		return false
	}
	return p.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// kill sends a signal and reaps the process. It is safe to call twice.
func (p *process) kill(sig os.Signal) {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
	p.waited.Do(func() { p.status = p.cmd.Wait() })
}

// press sends keys to the process, which for the TUI is someone typing.
func (p *process) press(keys string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, keys); err != nil {
		p.t.Fatalf("send %q: %v", keys, err)
	}
}

// snapshot writes the last frame the process drew to the test log, under -v.
func (p *process) snapshot(label string) {
	if !testing.Verbose() {
		return
	}
	p.t.Logf("\n=== %s (pid %d) ===\n%s", label, p.pid(), lastFrame(p.frames()))
}

// frames is everything the process drew, with the escape sequences removed.
func (p *process) frames() string { return stripStyles(p.out.String()) }

// waitOutput waits until the process has drawn something containing want.
func (p *process) waitOutput(what, want string, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.frames(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %s (%q). The process drew:\n%s", what, want, p.frames())
}

// startDaemonProcess runs `syngo daemon run` as its own process, with its
// log going to a file, which is what the TUI's --log-file tails.
func startDaemonProcess(t *testing.T, bin, dataDir, logPath string) *process {
	t.Helper()
	logFile, err := os.Create(logPath) //nolint:gosec // a path this test made
	if err != nil {
		t.Fatalf("create the daemon log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	p := start(t, bin, logFile, "--data-dir", dataDir, "--log-format", "json", "daemon", "run")
	p.waitOutput("the daemon to answer", "syngo daemon running", daemonTimeout)
	return p
}

// statusOf reads `status --json` with the binary and decodes the part of it
// the reconnect test asserts on.
func tuiStatusOf(t *testing.T, bin, dataDir string) tuiStatus {
	t.Helper()
	return decodeStatus(t, syngo(t, bin, dataDir, "--json", "status"))
}

// waitDaemon waits until a data directory has a daemon answering, and returns
// its report.
func waitDaemon(t *testing.T, bin, dataDir string) tuiStatus {
	t.Helper()
	deadline := time.Now().Add(daemonTimeout)
	for time.Now().Before(deadline) {
		report := tuiStatusOf(t, bin, dataDir)
		if report.Daemon.Running {
			return report
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no daemon answered for %s", dataDir)
	return tuiStatus{}
}

// tighteningArgs are the settings that make a cycle happen inside a test
// rather than at the shipped ten-minute and thirty-second defaults.
func tighteningArgs() []string {
	return []string{
		"config", "set",
		"--rescan-interval", testRescanInterval.String(),
		"--sync-interval", testSyncInterval.String(),
	}
}
