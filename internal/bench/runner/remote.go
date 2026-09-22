package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// remoteListenPort is the port the peer machine's bench daemon listens on.
// It is deliberately not 22000: a bench run must not collide with, or be
// mistaken for, a syncthis the operator already runs on that machine.
const remoteListenPort = 22300

// remoteReadyTimeout bounds how long the driver waits for the daemon it
// started on the peer machine to answer its control socket.
const remoteReadyTimeout = 60 * time.Second

// remoteDevice drives syncthis on the peer machine over ssh: it places the
// binary, initializes a data directory, adds the folder, starts and stops the
// daemon, and reads status, file hashes and the tailnet path back as JSON.
//
// Every path it creates sits under dir, the one directory the operator named,
// so a run leaves nothing on the peer machine outside it.
type remoteDevice struct {
	host string
	// dir is the remote directory, absolute once resolve has run.
	dir  string
	exec commandRunner

	// id is the peer's device id, known after prepare.
	id string
	// listen is the address the peer's daemon bound, known after start.
	listen string
	// running reports whether this driver started the daemon and has not
	// stopped it yet, so stop is safe in a defer and safe to call twice.
	running bool
}

// newRemoteDevice validates the operator's host and directory without
// touching the network.
func newRemoteDevice(host, dir string, exec commandRunner) (*remoteDevice, error) {
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("--peer-host is required: the two-machine scenarios drive the peer over ssh")
	}
	if err := validateRemoteDir(dir); err != nil {
		return nil, err
	}
	return &remoteDevice{host: host, dir: strings.TrimSuffix(dir, "/"), exec: exec}, nil
}

// validateRemoteDir refuses a remote directory that would let a run write
// outside one operator-named place.
func validateRemoteDir(dir string) error {
	trimmed := strings.TrimSpace(dir)
	switch trimmed {
	case "":
		return fmt.Errorf("--peer-dir is required: every path a run creates on the peer machine sits under it")
	case "/", "~", "~/":
		return fmt.Errorf("--peer-dir %q is the whole home or root directory; name a directory of its own, for example ~/syncthis-bench-remote", dir)
	}
	for _, part := range strings.Split(strings.TrimPrefix(trimmed, "~/"), "/") {
		if part == ".." {
			return fmt.Errorf("--peer-dir %q escapes itself with %q; name a plain directory, for example ~/syncthis-bench-remote", dir, "..")
		}
	}
	return nil
}

// paths under the remote directory.
func (r *remoteDevice) bin() string     { return r.dir + "/bin/syncthis" }
func (r *remoteDevice) dataDir() string { return r.dir + "/data" }
func (r *remoteDevice) root() string    { return r.dir + "/root" }
func (r *remoteDevice) pidFile() string { return r.dir + "/daemon.pid" }
func (r *remoteDevice) logFile() string { return r.dir + "/daemon.log" }

// run executes one shell command line on the peer machine.
func (r *remoteDevice) run(ctx context.Context, script string) ([]byte, error) {
	return r.exec.Run(ctx, "ssh", sshArgs(r.host, script)...)
}

// syncthis runs the peer's syncthis binary against the remote data
// directory.
func (r *remoteDevice) syncthis(ctx context.Context, args ...string) ([]byte, error) {
	return r.run(ctx, shellJoin(append([]string{r.bin(), "--data-dir", r.dataDir()}, args...)...))
}

// resolve checks that the peer answers ssh and turns a home-relative
// directory into an absolute one, so every later command names one place.
func (r *remoteDevice) resolve(ctx context.Context) error {
	out, err := r.run(ctx, "pwd")
	if err != nil {
		return fmt.Errorf("peer host %q does not answer ssh (try `ssh %s true` first; "+
			"the two-machine scenarios need key-based, passphrase-free ssh): %w", r.host, r.host, err)
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return fmt.Errorf("peer host %q answered ssh but reported no home directory", r.host)
	}
	switch {
	case strings.HasPrefix(r.dir, "/"):
	case strings.HasPrefix(r.dir, "~/"):
		r.dir = home + "/" + strings.TrimPrefix(r.dir, "~/")
	default:
		r.dir = home + "/" + r.dir
	}
	return validateResolvedDir(r.dir)
}

// validateResolvedDir is the last check before a run deletes anything on the
// peer machine: the resolved directory must be absolute and at least two
// levels below the root, so "rm -rf" under it can never name "/", "/home" or
// a home directory itself.
func validateResolvedDir(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("the peer directory resolved to %q, which is not an absolute path", dir)
	}
	var parts []string
	for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return fmt.Errorf("the peer directory resolved to %q, which climbs out of itself", dir)
		}
		parts = append(parts, part)
	}
	if len(parts) < 2 {
		return fmt.Errorf("the peer directory resolved to %q, too close to the root of the peer machine "+
			"for a run to clear it; name a directory of its own, for example ~/syncthis-bench-remote", dir)
	}
	return nil
}

// reset clears the folder root and the data directory the last run left on
// the peer and recreates them empty, so every run starts from a known state.
//
// Without it a run inherits whatever an earlier run left there: the peer
// serves that content back, this machine pulls it, and the recorded
// convergence time and byte count describe data that is not the corpus under
// test. It deletes exactly two paths, both under the directory the operator
// named and both checked by validateResolvedDir, and nothing else on the peer
// machine is touched.
func (r *remoteDevice) reset(ctx context.Context) error {
	if err := validateResolvedDir(r.dir); err != nil {
		return err
	}
	if _, err := r.run(ctx, shellJoin("rm", "-rf", r.root(), r.dataDir())); err != nil {
		return fmt.Errorf("clear the previous run's state under %s on %s: %w", r.dir, r.host, err)
	}
	if _, err := r.run(ctx, shellJoin("mkdir", "-p", r.dir+"/bin", r.dataDir(), r.root())); err != nil {
		return fmt.Errorf("create %s on %s: %w", r.dir, r.host, err)
	}
	return nil
}

// buildFunc compiles a syncthis binary for the given platform into dest.
type buildFunc func(ctx context.Context, dest, goos, goarch string) error

// prepare places the binary, creates the directories and initializes the data
// directory, and records the peer's device id. build is asked for a binary
// matching the platform the peer reports, because a binary for the wrong
// operating system or architecture would fail on the peer with nothing but an
// exec error to say why.
func (r *remoteDevice) prepare(ctx context.Context, localBinary string, build buildFunc) error {
	if err := r.resolve(ctx); err != nil {
		return err
	}
	out, err := r.run(ctx, "uname -s -m")
	if err != nil {
		return fmt.Errorf("read the platform of %s: %w", r.host, err)
	}
	goos, goarch, err := goPlatform(string(out))
	if err != nil {
		return err
	}
	if err := build(ctx, localBinary, goos, goarch); err != nil {
		return err
	}
	if err := r.reset(ctx); err != nil {
		return err
	}
	if _, err := r.exec.Run(ctx, "scp", scpArgs(localBinary, r.host, r.bin())...); err != nil {
		return fmt.Errorf("copy the syncthis binary to %s: %w", r.host, err)
	}
	if _, err := r.run(ctx, shellJoin("chmod", "+x", r.bin())); err != nil {
		return fmt.Errorf("make %s executable: %w", r.bin(), err)
	}

	initOut, err := r.syncthis(ctx, "init",
		"--name", "bench-"+r.host,
		"--listen", fmt.Sprintf("127.0.0.1:%d", remoteListenPort),
		"--json")
	if err != nil {
		return fmt.Errorf("initialize the peer's data directory: %w", err)
	}
	var initReport struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(initOut, &initReport); err != nil {
		return fmt.Errorf("read the peer's init report: %w: %s", err, strings.TrimSpace(string(initOut)))
	}
	if initReport.DeviceID == "" {
		return fmt.Errorf("the peer's init report carries no device id: %s", strings.TrimSpace(string(initOut)))
	}
	r.id = initReport.DeviceID
	return nil
}

// addFolder shares the remote root as FolderID. An id already configured from
// an earlier run is not an error.
func (r *remoteDevice) addFolder(ctx context.Context) error {
	out, err := r.syncthis(ctx, "folder", "add", r.root(), "--id", string(FolderID), "--json")
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), string(FolderID)) || strings.Contains(err.Error(), "already") {
		return nil
	}
	return fmt.Errorf("add the folder on %s: %w", r.host, err)
}

// trust stores the local device on the peer, the half of pairing the peer
// machine has to run itself.
func (r *remoteDevice) trust(ctx context.Context, localAddr, localID string) error {
	if _, err := r.syncthis(ctx, "peer", "add", localAddr,
		"--device-id", localID, "--yes", "--json"); err != nil {
		return fmt.Errorf("pair %s with this machine at %s: %w", r.host, localAddr, err)
	}
	return nil
}

// start launches the daemon on the peer's tailnet interface and waits until
// it answers status. The daemon is detached from the ssh session and its pid
// is written under the remote directory, so stop can always find it.
func (r *remoteDevice) start(ctx context.Context, iface string, poll time.Duration) error {
	script := strings.Join([]string{
		"set -e",
		shellJoin("mkdir", "-p", r.dir),
		"nohup " + shellJoin(r.bin(), "--data-dir", r.dataDir(), "daemon", "run",
			"--interface", iface,
			"--listen", fmt.Sprintf("127.0.0.1:%d", remoteListenPort)) +
			" >> " + shellQuote(r.logFile()) + " 2>&1 < /dev/null &",
		"echo $! > " + shellQuote(r.pidFile()),
	}, "\n")
	// The flag is set before the call, not after: ssh can start the daemon
	// and still fail, and a stop that skipped it would leave a process
	// running on the peer machine. stop is safe when there is nothing to
	// kill.
	r.running = true
	if _, err := r.run(ctx, script); err != nil {
		return fmt.Errorf("start the daemon on %s: %w", r.host, err)
	}

	deadline := time.Now().Add(remoteReadyTimeout)
	for {
		report, err := r.status(ctx)
		if err == nil && report.Daemon.Running && report.Daemon.Listen != "" {
			r.listen = report.Daemon.Listen
			return nil
		}
		if time.Now().After(deadline) {
			log, _ := r.run(ctx, shellJoin("tail", "-n", "20", r.logFile()))
			return fmt.Errorf("the daemon on %s did not become ready within %s; %s says:\n%s",
				r.host, remoteReadyTimeout, r.logFile(), strings.TrimSpace(string(log)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// stop kills the daemon this driver started and waits for it to go. It is
// safe to call in a defer and safe to call twice, so a failed run never
// leaves a daemon behind on the peer machine.
func (r *remoteDevice) stop(ctx context.Context) error {
	if !r.running {
		return nil
	}
	r.running = false
	script := strings.Join([]string{
		"pid=$(cat " + shellQuote(r.pidFile()) + " 2>/dev/null || true)",
		`if [ -n "$pid" ]; then`,
		`  kill "$pid" 2>/dev/null || true`,
		`  for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do`,
		`    kill -0 "$pid" 2>/dev/null || break`,
		`    sleep 1`,
		`  done`,
		`  kill -9 "$pid" 2>/dev/null || true`,
		"fi",
		"rm -f " + shellQuote(r.pidFile()),
	}, "\n")
	if _, err := r.run(ctx, script); err != nil {
		return fmt.Errorf("stop the daemon on %s (check %s by hand): %w", r.host, r.pidFile(), err)
	}
	return nil
}

// remoteStatus is the part of `syncthis status --json` a bench run reads back
// from the peer machine.
type remoteStatus struct {
	Daemon struct {
		Running bool   `json:"running"`
		Listen  string `json:"listen"`
	} `json:"daemon"`
	Folders []struct {
		ID             string `json:"id"`
		Files          int64  `json:"files"`
		PendingChanges int    `json:"pending_changes"`
	} `json:"folders"`
	Peers []struct {
		ID        string `json:"id"`
		Connected bool   `json:"connected"`
	} `json:"peers"`
	Transfers struct {
		Active  int `json:"active"`
		Pending int `json:"pending"`
	} `json:"transfers"`
}

// status reads the peer's status document.
func (r *remoteDevice) status(ctx context.Context) (remoteStatus, error) {
	out, err := r.syncthis(ctx, "status", "--json")
	if err != nil {
		return remoteStatus{}, err
	}
	return parseRemoteStatus(out)
}

// parseRemoteStatus decodes what `syncthis status --json` printed on the peer.
func parseRemoteStatus(out []byte) (remoteStatus, error) {
	var report remoteStatus
	if err := json.Unmarshal(out, &report); err != nil {
		return remoteStatus{}, fmt.Errorf("read the peer's status document: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return report, nil
}

// state summarizes the peer side for the convergence check.
func (r *remoteDevice) state(ctx context.Context) (sideState, error) {
	report, err := r.status(ctx)
	if err != nil {
		return sideState{}, err
	}
	return remoteSideState(report), nil
}

// remoteSideState turns a peer status document into the comparable summary.
func remoteSideState(report remoteStatus) sideState {
	s := sideState{
		active:  report.Transfers.Active,
		pending: report.Transfers.Pending,
	}
	for _, f := range report.Folders {
		if f.ID == string(FolderID) {
			s.files = f.Files
			s.pendingChanges = f.PendingChanges
		}
	}
	return s
}

// hashes returns the content hash of every file under the peer's folder root,
// keyed by path relative to that root.
func (r *remoteDevice) hashes(ctx context.Context) (map[string]string, error) {
	out, err := r.run(ctx, remoteHashScript(r.root()))
	if err != nil {
		return nil, fmt.Errorf("hash the folder on %s: %w", r.host, err)
	}
	return parseHashLines(string(out))
}

// remoteHashScript is the shell command that prints "<sha256>  ./<path>" for
// every regular file under root, skipping the ".syncthis" state directory,
// which is bookkeeping rather than synchronized content.
func remoteHashScript(root string) string {
	return "cd " + shellQuote(root) +
		` && find . -path ./.syncthis -prune -o -type f -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum`
}

// tailscaleStatus reads `tailscale status --json` on the peer machine.
func (r *remoteDevice) tailscaleStatus(ctx context.Context) ([]byte, error) {
	out, err := r.run(ctx, shellJoin("tailscale", "status", "--json"))
	if err != nil {
		return nil, fmt.Errorf("read the tailnet status on %s: %w", r.host, err)
	}
	return out, nil
}
