// Command syncthis-bench runs one benchmark scenario end to end, as
// described in docs/BENCH.md, and writes its result to the versioned results
// directory. It parses flags and calls internal/bench/runner; every decision
// about what a run does lives there.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wesbragagt/syncthis/internal/bench/runner"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("syncthis-bench", flag.ContinueOnError)
	fs.SetOutput(stderr)

	scenario := fs.String("scenario", "", "network scenario: lan, tailscale-direct or tailscale-derp (required)")
	name := fs.String("name", "default", "name of this run, used in the result filename")
	corpusDir := fs.String("corpus-dir", "bench/corpus/v1", "versioned corpus directory")
	resultsDir := fs.String("results-dir", "bench/results/v1", "versioned results directory")
	seed := fs.Int64("seed", 1, "seed for the corpus and the mutation set")

	dataDirA := fs.String("data-dir-a", "", "lan scenario: data directory of the first device (temp dir if empty)")
	dataDirB := fs.String("data-dir-b", "", "lan scenario: data directory of the second device (temp dir if empty)")

	peerAddr := fs.String("peer-addr", "", "tailscale scenarios: the already-running peer's listen address")
	peerDeviceID := fs.String("peer-device-id", "", "tailscale scenarios: the peer's device id, to pin its identity")
	localDataDir := fs.String("local-data-dir", "", "tailscale scenarios: this device's data directory")
	iface := fs.String("interface", "", "tailscale scenarios: network interface to bind the listener to")

	convergeTimeout := fs.Duration("converge-timeout", runner.DefaultConvergeTimeout, "how long to wait for convergence")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	parsedScenario, err := runner.ParseScenario(*scenario)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "syncthis-bench:", err)
		return 2
	}

	cfg := runner.Config{
		Scenario:        parsedScenario,
		Name:            *name,
		CorpusDir:       *corpusDir,
		ResultsDir:      *resultsDir,
		Seed:            *seed,
		DataDirA:        *dataDirA,
		DataDirB:        *dataDirB,
		PeerAddr:        *peerAddr,
		PeerDeviceID:    *peerDeviceID,
		LocalDataDir:    *localDataDir,
		Interface:       *iface,
		ConvergeTimeout: *convergeTimeout,
	}
	if err := runner.Validate(cfg); err != nil {
		_, _ = fmt.Fprintln(stderr, "syncthis-bench:", err)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	start := time.Now()
	outcome, err := runner.Run(ctx, cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "syncthis-bench:", err)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, outcome.ResultPath)
	_, _ = fmt.Fprintf(stdout, "scenario %s, name %s, took %s\n", cfg.Scenario, cfg.Name, time.Since(start).Round(time.Second))
	for _, line := range outcome.Unmeasured {
		_, _ = fmt.Fprintln(stdout, "unmeasured:", line)
	}
	return 0
}
