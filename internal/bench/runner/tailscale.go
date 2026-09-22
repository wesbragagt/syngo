package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/peers"
	"github.com/wesbragagt/syncthis/internal/bench/report"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// remotePollInterval is how often a two-machine run asks the peer machine for
// its status. Every poll is one ssh round trip, so it is far slower than the
// in-process polling the lan scenario uses.
const remotePollInterval = 250 * time.Millisecond

// remoteRecoveryObserveTimeout bounds how long a two-machine run watches the
// peer for a transfer actively in progress before it gives up on measuring a
// recovery. It is longer than the lan scenario's window because each check
// costs an ssh round trip.
const remoteRecoveryObserveTimeout = 30 * time.Second

// runTailscale runs the "tailscale-direct" or "tailscale-derp" scenario
// across two machines: this one, and the peer it drives over ssh. The peer's
// syncthis is placed, initialized, started and stopped by this process, so
// both sides of the pair are observable and convergence, recovery and the
// network path are measured rather than left to a second invocation.
//
// Every field this run cannot observe is left zero and explained in the
// returned Outcome.Unmeasured, never invented.
func runTailscale(ctx context.Context, cfg Config) (Outcome, error) {
	var unmeasured Unmeasured

	remote, err := newRemoteDevice(cfg.PeerHost, cfg.PeerDir, execRunner{})
	if err != nil {
		return Outcome{}, err
	}

	corpus, err := prepareCorpus(cfg)
	if err != nil {
		return Outcome{}, err
	}

	work, err := os.MkdirTemp("", "syncthis-bench-")
	if err != nil {
		return Outcome{}, fmt.Errorf("create working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	root := filepath.Join(work, "root")
	if err := copyTree(cfg.CorpusDir, root); err != nil {
		return Outcome{}, fmt.Errorf("copy corpus into folder root: %w", err)
	}

	startedAt := time.Now()
	before := readResourceSample()

	// The peer's daemon must be stopped even when this run fails partway
	// through, and the context this run was given may already be cancelled
	// by then, so the stop gets a context of its own.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := remote.stop(stopCtx); err != nil {
			fmt.Fprintln(os.Stderr, "syncthis-bench:", err)
		}
	}()

	if err := remote.prepare(ctx, filepath.Join(work, "syncthis"), buildStaticBinary); err != nil {
		return Outcome{}, err
	}
	if err := remote.addFolder(ctx); err != nil {
		return Outcome{}, err
	}
	if err := remote.start(ctx, cfg.Interface, remotePollInterval); err != nil {
		return Outcome{}, err
	}

	dataDir, cleanData, err := tempOrGiven(cfg.LocalDataDir, "syncthis-bench-local-data-")
	if err != nil {
		return Outcome{}, err
	}
	defer cleanData()

	n, err := startNode(ctx, "local", dataDir, root, nodeOptions{Interface: cfg.Interface})
	if err != nil {
		return Outcome{}, err
	}
	defer n.stop()

	detection, err := n.app.Network.Detect(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("checking the local Tailscale client: %w", err)
	}
	if !detection.Present || !detection.Running {
		return Outcome{}, fmt.Errorf("scenario %q needs a running Tailscale client on this machine "+
			"(tailscale status --json): present=%v running=%v err=%q; refusing rather than faking a "+
			"Tailscale measurement", cfg.Scenario, detection.Present, detection.Running, detection.Err)
	}

	peerID, err := device.ParseID(remote.id)
	if err != nil {
		return Outcome{}, fmt.Errorf("the peer reported an unreadable device id %q: %w", remote.id, err)
	}
	// The folder is shared before either side dials: a peer announces the
	// folders it has in its handshake, and the connection that was opened
	// before this device had the folder would never carry it.
	if err := n.addFolder(ctx, root); err != nil {
		return Outcome{}, err
	}
	if _, err := n.app.Peers.Add(ctx, peers.AddOptions{Address: remote.listen, ExpectID: peerID}); err != nil {
		return Outcome{}, fmt.Errorf("pairing with %s at %s: %w", remote.host, remote.listen, err)
	}
	if err := remote.trust(ctx, n.listen, n.id.String()); err != nil {
		return Outcome{}, err
	}

	localHost, _ := os.Hostname()
	observer := observeNetworkPath(ctx, networkPathObserver{
		local:     func(ctx context.Context) ([]byte, error) { return localTailscaleStatus(ctx) },
		remote:    remote.tailscaleStatus,
		localHost: localHost,
		peerHost:  remote.host,
		transferring: func(ctx context.Context) (bool, error) {
			state, err := remote.state(ctx)
			return state.active > 0, err
		},
	}, remotePollInterval)

	convergeStart := time.Now()
	err = waitConvergedRemote(ctx, n, remote, cfg.ConvergeTimeout, remotePollInterval)
	sample := observer.stop()
	if err != nil {
		return Outcome{}, fmt.Errorf("initial convergence: %w", err)
	}
	convergenceTime := time.Since(convergeStart)

	if sample.Path == "" {
		unmeasured.note("network_path", sample.Reason)
	}

	if _, err := applyMutationSet(root, mutationTargets(corpus.Files, cfg.Seed), cfg.Seed); err != nil {
		return Outcome{}, err
	}

	// Recovery is only measured when the peer was caught with a transfer
	// actively in progress: killing an idle daemon and restarting it times a
	// restart, not a recovery.
	observed, err := waitRemoteActiveTransfer(ctx, remote, remoteRecoveryObserveTimeout, remotePollInterval)
	if err != nil {
		return Outcome{}, fmt.Errorf("watching %s for an active transfer: %w", remote.host, err)
	}

	var recoveryTime time.Duration
	if observed {
		if err := remote.stop(ctx); err != nil {
			return Outcome{}, err
		}
		if err := remote.start(ctx, cfg.Interface, remotePollInterval); err != nil {
			return Outcome{}, fmt.Errorf("restart the daemon on %s: %w", remote.host, err)
		}
		recoveryStart := time.Now()
		if err := waitConvergedRemote(ctx, n, remote, cfg.ConvergeTimeout, remotePollInterval); err != nil {
			return Outcome{}, fmt.Errorf("convergence after recovery: %w", err)
		}
		recoveryTime = time.Since(recoveryStart)
	} else {
		unmeasured.note("recovery_time_ns", fmt.Sprintf(
			"no transfer was observed actively in progress on %s within %s of applying the mutation "+
				"set, so no kill was timed to hit one", remote.host, remoteRecoveryObserveTimeout))
		if err := waitConvergedRemote(ctx, n, remote, cfg.ConvergeTimeout, remotePollInterval); err != nil {
			return Outcome{}, fmt.Errorf("convergence after the mutation set: %w", err)
		}
	}

	after := readResourceSample()

	// The byte counter sums the transfers that completed on this machine.
	// In a two-machine run the corpus and the mutations start here and land
	// on the peer, so this side receives nothing, and the peer's journal
	// drops a transfer as soon as its file is installed: there is nothing
	// left to count once the run has converged.
	bytesTransferred := n.events.Bytes()
	if bytesTransferred == 0 {
		unmeasured.note("bytes_transferred",
			"every byte moved from this machine to "+remote.host+", and a completed transfer is "+
				"counted where it lands, so this side has nothing to report")
	}

	result := report.Result{
		Name:             cfg.Name,
		Scenario:         cfg.Scenario,
		Hardware:         hardwareProfile(),
		StartedAt:        startedAt,
		ConvergenceTime:  convergenceTime,
		BytesTransferred: bytesTransferred,
		RecoveryTime:     recoveryTime,
		NetworkPath:      sample.Path,
	}
	applyResourceDelta(&result, before, after, &unmeasured)

	return writeResult(cfg, result, unmeasured, observed)
}

// waitRemoteActiveTransfer polls the peer until its status shows a transfer
// actively in progress, or the timeout expires.
func waitRemoteActiveTransfer(ctx context.Context, r *remoteDevice, timeout, poll time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, err := r.state(ctx)
		if err != nil {
			return false, err
		}
		if state.active > 0 {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// localTailscaleStatus reads this machine's `tailscale status --json`.
func localTailscaleStatus(ctx context.Context) ([]byte, error) {
	return execRunner{}.Run(ctx, "tailscale", "status", "--json")
}
