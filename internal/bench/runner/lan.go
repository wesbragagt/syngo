package runner

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wesbragagt/syncthis/internal/bench/report"
)

// recoveryObserveTimeout bounds how long the recovery test waits for a real
// transfer to be actively in progress on b after the mutation set is
// applied. It exists to catch a kill mid-transfer, not to guarantee one:
// when nothing is observed moving within this bound, no daemon is killed and
// RecoveryTime is left unmeasured rather than reporting a plain restart as a
// recovery.
const recoveryObserveTimeout = 10 * time.Second

// recoveryObservePoll is how often the recovery test checks for an active
// transfer while it waits.
const recoveryObservePoll = 10 * time.Millisecond

// runLAN runs the "lan" scenario entirely in this process, over loopback,
// unattended.
func runLAN(ctx context.Context, cfg Config) (Outcome, error) {
	var unmeasured Unmeasured

	corpus, err := prepareCorpus(cfg)
	if err != nil {
		return Outcome{}, err
	}

	dataDirA, cleanA, err := tempOrGiven(cfg.DataDirA, "syncthis-bench-a-data-")
	if err != nil {
		return Outcome{}, err
	}
	defer cleanA()
	dataDirB, cleanB, err := tempOrGiven(cfg.DataDirB, "syncthis-bench-b-data-")
	if err != nil {
		return Outcome{}, err
	}
	defer cleanB()

	rootA, err := os.MkdirTemp("", "syncthis-bench-a-root-")
	if err != nil {
		return Outcome{}, fmt.Errorf("create root for a: %w", err)
	}
	defer func() { _ = os.RemoveAll(rootA) }()
	rootB, err := os.MkdirTemp("", "syncthis-bench-b-root-")
	if err != nil {
		return Outcome{}, fmt.Errorf("create root for b: %w", err)
	}
	defer func() { _ = os.RemoveAll(rootB) }()

	if err := copyTree(cfg.CorpusDir, rootA); err != nil {
		return Outcome{}, fmt.Errorf("copy corpus into folder root: %w", err)
	}

	startedAt := time.Now()
	before := readResourceSample()

	a, err := startNode(ctx, "a", dataDirA, rootA, nodeOptions{})
	if err != nil {
		return Outcome{}, err
	}
	defer a.stop()
	b, err := startNode(ctx, "b", dataDirB, rootB, nodeOptions{})
	if err != nil {
		return Outcome{}, err
	}
	defer b.stop()

	if err := a.trust(ctx, b); err != nil {
		return Outcome{}, fmt.Errorf("pair a with b: %w", err)
	}
	if err := b.trust(ctx, a); err != nil {
		return Outcome{}, fmt.Errorf("pair b with a: %w", err)
	}
	if err := a.addFolder(ctx, rootA); err != nil {
		return Outcome{}, err
	}
	if err := b.addFolder(ctx, rootB); err != nil {
		return Outcome{}, err
	}

	convergeStart := time.Now()
	if err := waitConverged(ctx, a, b, cfg.ConvergeTimeout, cfg.PollInterval); err != nil {
		return Outcome{}, fmt.Errorf("initial convergence: %w", err)
	}
	convergenceTime := time.Since(convergeStart)

	mutations, err := applyMutationSet(rootA, mutationTargets(corpus.Files, cfg.Seed), cfg.Seed)
	if err != nil {
		return Outcome{}, err
	}
	_ = mutations

	// Recovery is only measured when a real transfer was caught actively in
	// progress on b: the run's answer to "kill one daemon mid-transfer,
	// restart it, and measure the time until the folders converge again."
	// A mutation set small enough to finish syncing before this wait
	// returns is not a failure; it just means there is nothing to recover
	// from and no recovery number to report.
	observed, err := waitActiveTransfer(ctx, b, recoveryObserveTimeout, recoveryObservePoll)
	if err != nil {
		return Outcome{}, fmt.Errorf("watching b for an active transfer: %w", err)
	}

	var recoveryTime time.Duration
	bytesTransferred := a.events.Bytes()
	if observed {
		bytesBeforeRestart := b.events.Bytes()
		b.stop()
		b, err = startNode(ctx, "b", dataDirB, rootB, nodeOptions{})
		if err != nil {
			return Outcome{}, fmt.Errorf("restart b: %w", err)
		}
		defer b.stop()
		// b's data directory persists its identity, its trust of a and its
		// folder configuration, so a restart needs none of that redone.

		recoveryStart := time.Now()
		if err := waitConverged(ctx, a, b, cfg.ConvergeTimeout, cfg.PollInterval); err != nil {
			return Outcome{}, fmt.Errorf("convergence after recovery: %w", err)
		}
		recoveryTime = time.Since(recoveryStart)
		bytesTransferred += bytesBeforeRestart + b.events.Bytes()
	} else {
		unmeasured.note("recovery_time_ns", fmt.Sprintf(
			"no transfer was observed actively in progress on b within %s of applying the mutation "+
				"set, so no kill was timed to hit one; the mutation set finished syncing before a "+
				"recovery could be measured", recoveryObserveTimeout))
		if err := waitConverged(ctx, a, b, cfg.ConvergeTimeout, cfg.PollInterval); err != nil {
			return Outcome{}, fmt.Errorf("convergence after the mutation set: %w", err)
		}
		bytesTransferred += b.events.Bytes()
	}

	after := readResourceSample()

	result := report.Result{
		Name:             cfg.Name,
		Scenario:         cfg.Scenario,
		Hardware:         hardwareProfile(),
		StartedAt:        startedAt,
		ConvergenceTime:  convergenceTime,
		BytesTransferred: bytesTransferred,
		RecoveryTime:     recoveryTime,
	}
	applyResourceDelta(&result, before, after, &unmeasured)

	return writeResult(cfg, result, unmeasured, observed)
}
