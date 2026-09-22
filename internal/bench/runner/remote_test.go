package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedCommand is one program the driver asked to run.
type recordedCommand struct {
	name string
	args []string
}

// line is the command as it would have been typed, for assertions.
func (c recordedCommand) line() string {
	return c.name + " " + strings.Join(c.args, " ")
}

// fakeRunner answers commands from a table instead of running them, so every
// test in this file stays off the network. A command with no entry in the
// table returns empty output and no error.
type fakeRunner struct {
	mu    sync.Mutex
	calls []recordedCommand
	// answer maps a substring of the command line to what running it
	// produces.
	answer func(cmd recordedCommand) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := recordedCommand{name: name, args: args}
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	if f.answer == nil {
		return nil, nil
	}
	return f.answer(cmd)
}

func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.line()
	}
	return out
}

// statusJSON is what the peer's `syncthis status --json` prints in these
// tests: a running daemon with an idle bench folder.
const statusJSON = `{
  "schema_version": 1,
  "daemon": {"running": true, "pid": 42, "listen": "100.64.0.7:22300"},
  "folders": [{"id": "bench", "files": 3, "pending_changes": 0}],
  "peers": [{"id": "PEER", "connected": true}],
  "transfers": {"active": 0, "pending": 0}
}`

// peerAnswers is the table a healthy peer machine would produce.
func peerAnswers(cmd recordedCommand) ([]byte, error) {
	line := cmd.line()
	switch {
	case strings.HasSuffix(line, "pwd"):
		return []byte("/home/bench\n"), nil
	case strings.HasSuffix(line, "uname -s -m"):
		return []byte("Linux x86_64\n"), nil
	case strings.Contains(line, "'init'"):
		return []byte(`{"device_id": "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRST"}`), nil
	case strings.Contains(line, "'status'"):
		return []byte(statusJSON), nil
	default:
		return nil, nil
	}
}

// newTestRemote returns a driver wired to fake, already resolved.
func newTestRemote(t *testing.T, fake *fakeRunner) *remoteDevice {
	t.Helper()
	r, err := newRemoteDevice("macpro-nixos", "~/syncthis-bench-remote", fake)
	if err != nil {
		t.Fatalf("newRemoteDevice: %v", err)
	}
	return r
}

func TestNewRemoteDeviceRefusesAMissingPeerHost(t *testing.T) {
	if _, err := newRemoteDevice("", "~/syncthis-bench-remote", &fakeRunner{}); err == nil {
		t.Fatal("newRemoteDevice with no host = nil, want a refusal")
	}
	if _, err := newRemoteDevice("   ", "~/syncthis-bench-remote", &fakeRunner{}); err == nil {
		t.Fatal("newRemoteDevice with a blank host = nil, want a refusal")
	}
}

func TestNewRemoteDeviceRefusesAnEscapingDirectory(t *testing.T) {
	for _, dir := range []string{"", "/", "~", "~/", "~/bench/../elsewhere", "../bench"} {
		if _, err := newRemoteDevice("macpro-nixos", dir, &fakeRunner{}); err == nil {
			t.Errorf("newRemoteDevice with dir %q = nil, want a refusal", dir)
		}
	}
}

func TestSSHArgsRunOneScriptInBatchMode(t *testing.T) {
	args := sshArgs("macpro-nixos", "pwd")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Errorf("ssh args %q carry no BatchMode=yes; a run must fail rather than wait for a prompt", joined)
	}
	if args[len(args)-1] != "pwd" || args[len(args)-2] != "macpro-nixos" {
		t.Errorf("ssh args = %q, want the host then the script last", args)
	}
}

func TestSCPArgsNameTheRemotePath(t *testing.T) {
	args := scpArgs("/tmp/syncthis", "macpro-nixos", "/home/bench/dir/bin/syncthis")
	if got := args[len(args)-1]; got != "macpro-nixos:/home/bench/dir/bin/syncthis" {
		t.Errorf("scp destination = %q, want host:path", got)
	}
	if got := args[len(args)-2]; got != "/tmp/syncthis" {
		t.Errorf("scp source = %q, want the local binary", got)
	}
}

func TestShellJoinQuotesEveryWord(t *testing.T) {
	got := shellJoin("echo", "a b", "it's")
	want := `'echo' 'a b' 'it'\''s'`
	if got != want {
		t.Errorf("shellJoin = %s, want %s", got, want)
	}
}

func TestPrepareKeepsEveryPathUnderTheOperatorsDirectory(t *testing.T) {
	fake := &fakeRunner{answer: peerAnswers}
	r := newTestRemote(t, fake)

	var builtFor string
	build := func(_ context.Context, dest, goos, goarch string) error {
		builtFor = goos + "/" + goarch
		if dest == "" {
			t.Error("build was asked for an empty destination")
		}
		return nil
	}
	if err := r.prepare(context.Background(), "/tmp/local-syncthis", build); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if builtFor != "linux/amd64" {
		t.Errorf("built for %q, want the platform the peer reported (linux/amd64)", builtFor)
	}
	if r.id == "" {
		t.Error("prepare recorded no device id for the peer")
	}
	const dir = "/home/bench/syncthis-bench-remote"
	if r.dir != dir {
		t.Fatalf("remote dir = %q, want %q", r.dir, dir)
	}
	for _, line := range fake.lines() {
		if strings.HasSuffix(line, "pwd") || strings.HasSuffix(line, "uname -s -m") {
			continue
		}
		if !strings.Contains(line, dir) {
			t.Errorf("command %q names no path under %s", line, dir)
		}
	}
}

func TestPrepareRefusesAnUnreachablePeer(t *testing.T) {
	fake := &fakeRunner{answer: func(recordedCommand) ([]byte, error) {
		return nil, errors.New("ssh: connect to host macpro-nixos port 22: No route to host")
	}}
	r := newTestRemote(t, fake)

	err := r.prepare(context.Background(), "/tmp/local-syncthis", func(context.Context, string, string, string) error {
		t.Error("prepare built a binary for a peer it could not reach")
		return nil
	})
	if err == nil {
		t.Fatal("prepare against an unreachable peer = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "ssh") || !strings.Contains(err.Error(), "macpro-nixos") {
		t.Errorf("error %q does not say which host did not answer ssh", err)
	}
	if len(fake.lines()) != 1 {
		t.Errorf("prepare ran %d commands after the first one failed, want 1: %v", len(fake.lines()), fake.lines())
	}
}

func TestStartAndStopDriveTheDaemonTheRunnerOwns(t *testing.T) {
	fake := &fakeRunner{answer: peerAnswers}
	r := newTestRemote(t, fake)
	if err := r.prepare(context.Background(), "/tmp/local-syncthis", func(context.Context, string, string, string) error { return nil }); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := r.start(context.Background(), "tailscale0", time.Millisecond); err != nil {
		t.Fatalf("start: %v", err)
	}
	if r.listen != "100.64.0.7:22300" {
		t.Errorf("listen = %q, want the address the peer's status reported", r.listen)
	}

	started := findCommand(t, fake, "daemon")
	for _, want := range []string{"tailscale0", r.dataDir(), r.pidFile()} {
		if !strings.Contains(started, want) {
			t.Errorf("start command %q does not mention %q", started, want)
		}
	}

	if err := r.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped := findCommand(t, fake, "kill")
	if !strings.Contains(stopped, r.pidFile()) {
		t.Errorf("stop command %q does not kill the pid this run recorded", stopped)
	}

	// A second stop must not kill anything: the daemon is already gone, and
	// stop is called from a defer as well as from the recovery measurement.
	before := len(fake.lines())
	if err := r.stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if len(fake.lines()) != before {
		t.Errorf("a second stop ran %d more commands, want none", len(fake.lines())-before)
	}
}

// findCommand returns the last command line holding substr.
func findCommand(t *testing.T, fake *fakeRunner, substr string) string {
	t.Helper()
	for i := len(fake.lines()) - 1; i >= 0; i-- {
		if strings.Contains(fake.lines()[i], substr) {
			return fake.lines()[i]
		}
	}
	t.Fatalf("no command holding %q was run: %v", substr, fake.lines())
	return ""
}

func TestRemoteStatusIsSummarizedForTheConvergenceCheck(t *testing.T) {
	report, err := parseRemoteStatus([]byte(statusJSON))
	if err != nil {
		t.Fatalf("parseRemoteStatus: %v", err)
	}
	got := remoteSideState(report)
	want := sideState{files: 3}
	if got != want {
		t.Errorf("remoteSideState = %+v, want %+v", got, want)
	}
	if !got.settled() {
		t.Error("a side with nothing in flight and nothing pending is not settled")
	}
	if (sideState{active: 1}).settled() {
		t.Error("a side with an active transfer counts as settled")
	}
	if (sideState{pendingChanges: 2}).settled() {
		t.Error("a side that failed to apply two changes counts as settled")
	}
}

func TestRemoteStatusRefusesGarbage(t *testing.T) {
	if _, err := parseRemoteStatus([]byte("bash: syncthis: command not found")); err == nil {
		t.Fatal("parseRemoteStatus of a shell error = nil, want an error")
	}
}

func TestRemoteHashScriptSkipsTheStateDirectory(t *testing.T) {
	script := remoteHashScript("/home/bench/dir/root")
	for _, want := range []string{"sha256sum", ".syncthis", "/home/bench/dir/root"} {
		if !strings.Contains(script, want) {
			t.Errorf("hash script %q does not mention %q", script, want)
		}
	}
}
