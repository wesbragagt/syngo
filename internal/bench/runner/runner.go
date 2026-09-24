// Package runner executes one benchmark scenario end to end, as described in
// docs/BENCH.md: it generates the corpus, starts the devices a scenario
// needs, pairs them, shares a folder, waits for convergence, applies the
// mutation set, waits again, measures a recovery, and writes one
// internal/bench/report.Result to the versioned results directory.
//
// The "lan" scenario is fully self-contained: it starts two devices in this
// process and connects them over loopback, so Run needs nothing beyond the
// defaults. The "tailscale-direct" and "tailscale-derp" scenarios span two
// machines on one tailnet: Run drives the local side in this process and the
// peer machine over ssh, so both sides are observable and convergence,
// recovery and the network path are measured rather than guessed. A field
// neither side could observe is left zero and explained, never invented.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wesbragagt/syngo/internal/bench/fixture"
	"github.com/wesbragagt/syngo/internal/bench/report"
)

// DefaultTailnetInterface is the interface both daemons bind to in the
// Tailscale scenarios when the operator names no other one.
const DefaultTailnetInterface = "tailscale0"

// DefaultConvergeTimeout bounds how long Run waits for a folder to converge,
// once for the initial corpus and once after the mutation set.
const DefaultConvergeTimeout = 5 * time.Minute

// DefaultPollInterval is how often Run checks whether a folder has
// converged.
const DefaultPollInterval = 100 * time.Millisecond

// Config configures one run of one scenario.
type Config struct {
	// Scenario is the network setup to measure; see
	// docs/BENCH.md#network-scenarios.
	Scenario report.Scenario
	// Name identifies the run inside the results directory, for example
	// "baseline".
	Name string
	// CorpusDir is the versioned corpus directory the run reads its fixture
	// from, for example "bench/corpus/v1". Generate only writes here when
	// the directory is empty; an existing corpus is reused, matching
	// docs/BENCH.md's "regenerated rarely".
	CorpusDir string
	// ResultsDir is the versioned results directory the run writes its JSON
	// result to, for example "bench/results/v1".
	ResultsDir string
	// Seed makes the corpus and the mutations reproducible.
	Seed int64
	// Spec overrides the corpus shape. The zero value takes
	// fixture.DefaultSpec(Seed).
	Spec fixture.Spec

	// DataDirA and DataDirB are the data directories of the two devices the
	// "lan" scenario starts in this process. Empty means a temporary
	// directory is created and removed when the run ends.
	DataDirA, DataDirB string

	// PeerHost is the peer machine, as ssh names it: a host in
	// ~/.ssh/config, a tailnet name or an address. Required for
	// "tailscale-direct" and "tailscale-derp"; refused for "lan".
	PeerHost string
	// PeerDir is the one directory on the peer machine a run may create
	// anything under: the binary, the data directory, the folder root, the
	// daemon's pid file and its log. Required alongside PeerHost.
	PeerDir string
	// LocalDataDir is this device's data directory for the Tailscale
	// scenarios. Empty means a temporary directory is created and removed
	// when the run ends. Refused for "lan".
	LocalDataDir string
	// Interface is the network interface both daemons bind their listener
	// to, the rule docs/ARCHITECTURE.md gives the Tailscale scenarios for
	// reaching the tailnet rather than the LAN. It names the interface on
	// both machines; it defaults to DefaultTailnetInterface.
	Interface string

	// ConvergeTimeout overrides DefaultConvergeTimeout.
	ConvergeTimeout time.Duration
	// PollInterval overrides DefaultPollInterval.
	PollInterval time.Duration
}

// withDefaults fills in the timings Config leaves at zero.
func (c Config) withDefaults() Config {
	if c.ConvergeTimeout == 0 {
		c.ConvergeTimeout = DefaultConvergeTimeout
	}
	if c.PollInterval == 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.Name == "" {
		c.Name = "default"
	}
	if c.Spec == (fixture.Spec{}) {
		c.Spec = fixture.DefaultSpec(c.Seed)
	}
	if c.Interface == "" && c.Scenario != report.ScenarioLAN {
		c.Interface = DefaultTailnetInterface
	}
	return c
}

// Validate reports whether cfg can be run at all, without touching the
// filesystem or the network. It is what a runner checks before starting
// anything, so a missing Tailscale setup is refused with one clear message
// rather than failing partway through.
func Validate(cfg Config) error {
	switch cfg.Scenario {
	case report.ScenarioLAN:
		if cfg.PeerHost != "" || cfg.PeerDir != "" || cfg.LocalDataDir != "" {
			return fmt.Errorf("scenario %q runs on one machine over loopback; "+
				"--peer-host, --peer-dir and --local-data-dir are for the "+
				"tailscale scenarios and must be left unset", cfg.Scenario)
		}
	case report.ScenarioTailscaleDirect, report.ScenarioTailscaleDERP:
		var missing []string
		if cfg.PeerHost == "" {
			missing = append(missing, "--peer-host")
		}
		if cfg.PeerDir == "" {
			missing = append(missing, "--peer-dir")
		}
		if len(missing) > 0 {
			return fmt.Errorf("scenario %q measures two machines: it drives the peer over ssh, "+
				"so it needs %v (see docs/BENCH.md#tailscale-direct-and-tailscale-derp for what "+
				"the peer machine must have ready); refusing rather than faking a Tailscale "+
				"measurement", cfg.Scenario, missing)
		}
		if _, err := newRemoteDevice(cfg.PeerHost, cfg.PeerDir, execRunner{}); err != nil {
			return err
		}
		if cfg.DataDirA != "" || cfg.DataDirB != "" {
			return fmt.Errorf("scenario %q uses --local-data-dir, not --data-dir-a/--data-dir-b, "+
				"which are for the lan scenario's two in-process devices", cfg.Scenario)
		}
	default:
		return fmt.Errorf("unknown scenario %q, want one of %q, %q, %q",
			cfg.Scenario, report.ScenarioLAN, report.ScenarioTailscaleDirect, report.ScenarioTailscaleDERP)
	}
	if cfg.CorpusDir == "" {
		return errors.New("--corpus-dir is required")
	}
	if cfg.ResultsDir == "" {
		return errors.New("--results-dir is required")
	}
	return nil
}

// ParseScenario validates s against the three scenarios docs/BENCH.md
// defines.
func ParseScenario(s string) (report.Scenario, error) {
	switch sc := report.Scenario(s); sc {
	case report.ScenarioLAN, report.ScenarioTailscaleDirect, report.ScenarioTailscaleDERP:
		return sc, nil
	default:
		return "", fmt.Errorf("unknown scenario %q, want one of %q, %q, %q",
			s, report.ScenarioLAN, report.ScenarioTailscaleDirect, report.ScenarioTailscaleDERP)
	}
}

// Unmeasured is emitted, one line per field, describing a Result field this
// run left at zero and why. Run appends to it as it goes; a caller prints it
// alongside the result path so a zero is never mistaken for a measurement.
type Unmeasured []string

// note records that a field was left unmeasured.
func (u *Unmeasured) note(field, reason string) {
	*u = append(*u, fmt.Sprintf("%s: %s", field, reason))
}

// Outcome is what Run returns: the result it wrote, the path it wrote it to,
// and which fields it could not measure on this platform or in this
// scenario.
type Outcome struct {
	Result     report.Result
	ResultPath string
	Unmeasured Unmeasured
	// RecoveryObserved reports whether the run saw a real transfer actively
	// in progress before it killed a daemon to measure recovery. It is
	// false whenever RecoveryTime was left at zero because nothing was
	// caught in flight.
	RecoveryObserved bool
}

// Run executes cfg's scenario end to end and writes one result to
// cfg.ResultsDir.
func Run(ctx context.Context, cfg Config) (Outcome, error) {
	cfg = cfg.withDefaults()
	if err := Validate(cfg); err != nil {
		return Outcome{}, err
	}

	switch cfg.Scenario {
	case report.ScenarioLAN:
		return runLAN(ctx, cfg)
	default:
		return runTailscale(ctx, cfg)
	}
}

// prepareCorpus generates the corpus if cfg.CorpusDir is empty, and reuses it
// otherwise. It returns the corpus files fixture.Generate reported, reading
// them back from disk when the corpus already existed.
func prepareCorpus(cfg Config) (fixture.Result, error) {
	result, err := fixture.Generate(cfg.CorpusDir, cfg.Spec)
	if err == nil {
		return result, nil
	}
	if !isNotEmpty(err) {
		return fixture.Result{}, fmt.Errorf("generate corpus: %w", err)
	}
	// The corpus directory already holds a tree from an earlier run;
	// docs/BENCH.md says a corpus version is regenerated rarely, so this run
	// reuses what is there.
	return readExistingCorpus(cfg.CorpusDir)
}

// isNotEmpty reports whether err is fixture.Generate's "not empty" refusal.
func isNotEmpty(err error) bool {
	return err != nil && filepathErrLooksLikeNotEmpty(err.Error())
}

func filepathErrLooksLikeNotEmpty(msg string) bool {
	const suffix = "is not empty"
	return len(msg) >= len(suffix) && msg[len(msg)-len(suffix):] == suffix
}

// readExistingCorpus walks dir and reports every regular file it finds, as
// fixture.Generate would have, so a reused corpus can still be copied into a
// folder root and counted.
func readExistingCorpus(dir string) (fixture.Result, error) {
	var result fixture.Result
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		result.Files = append(result.Files, fixture.FileResult{
			Path: filepath.ToSlash(rel), Size: info.Size(),
		})
		return nil
	})
	if err != nil {
		return fixture.Result{}, fmt.Errorf("read existing corpus %s: %w", dir, err)
	}
	return result, nil
}

// copyTree copies every regular file under src into dst, creating
// directories as needed. It is how a run turns the read-only corpus into a
// folder root a device may write to (mutations, .syngo) without touching
// the corpus itself.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// tempOrGiven returns dir, or a fresh temporary directory under the standard
// location when dir is empty. The returned cleanup removes it only when one
// was created.
func tempOrGiven(dir, pattern string) (path string, cleanup func(), err error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", nil, fmt.Errorf("create %s: %w", dir, err)
		}
		return dir, func() {}, nil
	}
	path, err = os.MkdirTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	return path, func() { _ = os.RemoveAll(path) }, nil
}
