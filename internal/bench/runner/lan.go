package runner

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wesbragagt/syncthis/internal/bench/report"
)

// mutationSettleDelay is how long applyMutationSet's changes are given to
// reach the scanner and start a transfer before the recovery test kills a
// node. It is short on purpose: the point is to catch a transfer in flight,
// not to wait for it to finish.
const mutationSettleDelay = 300 * time.Millisecond

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

	// Kill b mid-transfer, restart it and measure how long recovery takes:
	// the run's answer to "kill one daemon mid-transfer, restart it, and
	// measure the time until the folders converge again."
	select {
	case <-time.After(mutationSettleDelay):
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
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
	recoveryTime := time.Since(recoveryStart)

	after := readResourceSample()
	bytesTransferred := a.events.Bytes() + bytesBeforeRestart + b.events.Bytes()

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

	return writeResult(cfg, result, unmeasured)
}
