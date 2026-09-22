package runner

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/peers"
	"github.com/wesbragagt/syncthis/internal/bench/report"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// tailscaleSettleDelay is how long a Tailscale run waits after sharing the
// folder and applying the mutation set before it writes its result. It gives
// the daemon time to dial the peer and start serving, but it is not a
// convergence wait: this process has no way to see the peer machine's
// folder, so it never claims one.
const tailscaleSettleDelay = 5 * time.Second

// runTailscale runs the "tailscale-direct" or "tailscale-derp" scenario. It
// only ever drives the local side of the pair: the peer is a real device on
// another machine, already reachable on the tailnet per docs/BENCH.md. Every
// field this process cannot observe from here is left zero and explained in
// the returned Outcome.Unmeasured, never invented.
func runTailscale(ctx context.Context, cfg Config) (Outcome, error) {
	var unmeasured Unmeasured

	corpus, err := prepareCorpus(cfg)
	if err != nil {
		return Outcome{}, err
	}

	root, err := os.MkdirTemp("", "syncthis-bench-root-")
	if err != nil {
		return Outcome{}, fmt.Errorf("create folder root: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := copyTree(cfg.CorpusDir, root); err != nil {
		return Outcome{}, fmt.Errorf("copy corpus into folder root: %w", err)
	}

	startedAt := time.Now()
	before := readResourceSample()

	n, err := startNode(ctx, "local", cfg.LocalDataDir, root, nodeOptions{Interface: cfg.Interface})
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

	peerID, err := device.ParseID(cfg.PeerDeviceID)
	if err != nil {
		return Outcome{}, fmt.Errorf("--peer-device-id: %w", err)
	}
	if _, err := n.app.Peers.Add(ctx, peers.AddOptions{Address: cfg.PeerAddr, ExpectID: peerID}); err != nil {
		return Outcome{}, fmt.Errorf("pairing with the peer at %s: %w", cfg.PeerAddr, err)
	}
	if err := n.addFolder(ctx, root); err != nil {
		return Outcome{}, err
	}

	if _, err := applyMutationSet(root, mutationTargets(corpus.Files, cfg.Seed), cfg.Seed); err != nil {
		return Outcome{}, err
	}

	select {
	case <-time.After(tailscaleSettleDelay):
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}

	after := readResourceSample()

	unmeasured.note("convergence_time_ns",
		"this process only drives one side of the pair and cannot read the peer machine's folder; "+
			"compare started_at across both machines' results instead")
	unmeasured.note("recovery_time_ns",
		"recovery needs killing and restarting the peer process, which runs on another machine; "+
			"this invocation only drives the local side")

	result := report.Result{
		Name:             cfg.Name,
		Scenario:         cfg.Scenario,
		Hardware:         hardwareProfile(),
		StartedAt:        startedAt,
		BytesTransferred: n.events.Bytes(),
	}
	applyResourceDelta(&result, before, after, &unmeasured)

	return writeResult(cfg, result, unmeasured)
}
