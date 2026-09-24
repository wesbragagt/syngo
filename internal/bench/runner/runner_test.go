package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/bench/fixture"
	"github.com/wesbragagt/syngo/internal/bench/report"
	"github.com/wesbragagt/syngo/internal/bench/runner"
)

func TestParseScenario(t *testing.T) {
	cases := []struct {
		in      string
		want    report.Scenario
		wantErr bool
	}{
		{"lan", report.ScenarioLAN, false},
		{"tailscale-direct", report.ScenarioTailscaleDirect, false},
		{"tailscale-derp", report.ScenarioTailscaleDERP, false},
		{"", "", true},
		{"wifi", "", true},
		{"LAN", "", true},
	}
	for _, c := range cases {
		got, err := runner.ParseScenario(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseScenario(%q) = %q, nil, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseScenario(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseScenario(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func baseConfig() runner.Config {
	return runner.Config{
		Scenario:   report.ScenarioLAN,
		Name:       "t",
		CorpusDir:  "corpus",
		ResultsDir: "results",
	}
}

func TestValidate_LAN(t *testing.T) {
	if err := runner.Validate(baseConfig()); err != nil {
		t.Fatalf("Validate(lan) = %v, want nil", err)
	}
}

func TestValidate_LANRefusesTailscaleFlags(t *testing.T) {
	cfg := baseConfig()
	cfg.PeerHost = "macpro-nixos"
	if err := runner.Validate(cfg); err == nil {
		t.Fatal("Validate(lan with --peer-host) = nil, want an error")
	}
}

func TestValidate_TailscaleNeedsPeerHostAndPeerDir(t *testing.T) {
	for _, scenario := range []report.Scenario{report.ScenarioTailscaleDirect, report.ScenarioTailscaleDERP} {
		cfg := baseConfig()
		cfg.Scenario = scenario
		err := runner.Validate(cfg)
		if err == nil {
			t.Fatalf("Validate(%s with nothing set) = nil, want a refusal naming the missing flags", scenario)
		}
		t.Logf("%s: %v", scenario, err)
	}
}

func TestValidate_TailscaleWithEverythingSet(t *testing.T) {
	cfg := baseConfig()
	cfg.Scenario = report.ScenarioTailscaleDirect
	cfg.PeerHost = "macpro-nixos"
	cfg.PeerDir = "~/syngo-bench-remote"
	cfg.LocalDataDir = t.TempDir()
	if err := runner.Validate(cfg); err != nil {
		t.Fatalf("Validate(tailscale-direct, fully configured) = %v, want nil", err)
	}
}

// A peer directory that names the whole home directory, or climbs out of
// itself, would let a run write anywhere on the peer machine.
func TestValidate_TailscaleRefusesAnEscapingPeerDir(t *testing.T) {
	for _, dir := range []string{"~", "~/", "/", "bench/../..", "~/bench/../elsewhere"} {
		cfg := baseConfig()
		cfg.Scenario = report.ScenarioTailscaleDirect
		cfg.PeerHost = "macpro-nixos"
		cfg.PeerDir = dir
		if err := runner.Validate(cfg); err == nil {
			t.Errorf("Validate with --peer-dir %q = nil, want a refusal", dir)
		}
	}
}

func TestValidate_TailscaleRefusesLANDataDirFlags(t *testing.T) {
	cfg := baseConfig()
	cfg.Scenario = report.ScenarioTailscaleDirect
	cfg.PeerHost = "macpro-nixos"
	cfg.PeerDir = "~/syngo-bench-remote"
	cfg.LocalDataDir = t.TempDir()
	cfg.DataDirA = t.TempDir()
	if err := runner.Validate(cfg); err == nil {
		t.Fatal("Validate(tailscale-direct with --data-dir-a) = nil, want an error")
	}
}

func TestValidate_UnknownScenario(t *testing.T) {
	cfg := baseConfig()
	cfg.Scenario = "wifi"
	if err := runner.Validate(cfg); err == nil {
		t.Fatal("Validate(wifi) = nil, want an error")
	}
}

func TestValidate_RequiresCorpusAndResultsDir(t *testing.T) {
	cfg := baseConfig()
	cfg.CorpusDir = ""
	if err := runner.Validate(cfg); err == nil {
		t.Fatal("Validate with no --corpus-dir = nil, want an error")
	}

	cfg = baseConfig()
	cfg.ResultsDir = ""
	if err := runner.Validate(cfg); err == nil {
		t.Fatal("Validate with no --results-dir = nil, want an error")
	}
}

// smallSpec keeps a run's corpus and its measured run time tiny.
func smallSpec(seed int64) fixture.Spec {
	return fixture.Spec{
		Seed:        seed,
		SmallFiles:  fixture.CountSize{Count: 6, MinSize: 20, MaxSize: 80},
		BinaryFiles: fixture.CountSize{Count: 2, MinSize: 16, MaxSize: 32},
		DeepTree:    fixture.DeepTreeSpec{Depth: 2, FilesPerDir: 1, FileSize: 20},
	}
}

// TestRun_LAN runs the lan scenario for real, over loopback, with a corpus
// small enough for `go test`. It is skipped under -short.
func TestRun_LAN(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two real daemons and syncs a folder; run without -short")
	}

	dir := t.TempDir()
	cfg := runner.Config{
		Scenario:        report.ScenarioLAN,
		Name:            "loopback-test",
		CorpusDir:       filepath.Join(dir, "corpus"),
		ResultsDir:      filepath.Join(dir, "results"),
		Seed:            7,
		Spec:            smallSpec(7),
		ConvergeTimeout: 30 * time.Second,
		PollInterval:    20 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	outcome, err := runner.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.ResultPath == "" {
		t.Fatal("outcome.ResultPath is empty")
	}
	if _, err := os.Stat(outcome.ResultPath); err != nil {
		t.Fatalf("result file: %v", err)
	}

	data, err := os.ReadFile(outcome.ResultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var result report.Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, data)
	}
	if result.Scenario != report.ScenarioLAN {
		t.Errorf("scenario = %q, want %q", result.Scenario, report.ScenarioLAN)
	}
	if result.ConvergenceTime <= 0 {
		t.Errorf("convergence_time_ns = %d, want > 0", result.ConvergenceTime)
	}
	if result.BytesTransferred <= 0 {
		t.Errorf("bytes_transferred = %d, want > 0", result.BytesTransferred)
	}

	// The recovery number is only trustworthy when the run actually caught a
	// transfer in flight before it killed a node. Either that happened, in
	// which case RecoveryTime must be positive and nothing calls it
	// unmeasured, or it did not, in which case RecoveryTime must stay zero
	// and the reason must be on record. A run that reports a positive
	// RecoveryTime without having observed a transfer, or that reports zero
	// without explaining why, is the bug this test exists to catch.
	notedUnmeasured := hasUnmeasured(outcome.Unmeasured, "recovery_time_ns")
	switch {
	case outcome.RecoveryObserved:
		if result.RecoveryTime <= 0 {
			t.Errorf("RecoveryObserved = true but recovery_time_ns = %d, want > 0", result.RecoveryTime)
		}
		if notedUnmeasured {
			t.Errorf("RecoveryObserved = true but recovery_time_ns is also marked unmeasured: %v", outcome.Unmeasured)
		}
	case !outcome.RecoveryObserved:
		if result.RecoveryTime != 0 {
			t.Errorf("RecoveryObserved = false but recovery_time_ns = %d, want 0: a kill was never timed to hit a transfer",
				result.RecoveryTime)
		}
		if !notedUnmeasured {
			t.Errorf("RecoveryObserved = false but nothing explains why recovery_time_ns is 0: %v", outcome.Unmeasured)
		}
	}
}

// hasUnmeasured reports whether unmeasured carries a line for field.
func hasUnmeasured(unmeasured []string, field string) bool {
	for _, line := range unmeasured {
		if strings.HasPrefix(line, field+":") {
			return true
		}
	}
	return false
}

// TestRun_TailscaleLive runs a real two-machine scenario. It reaches the
// network and drives another computer, so it never runs by default: set
// SYNGO_BENCH_PEER_HOST to the ssh host of a peer machine on the same
// tailnet, and optionally SYNGO_BENCH_PEER_DIR to the directory it may
// use there.
func TestRun_TailscaleLive(t *testing.T) {
	host := os.Getenv("SYNGO_BENCH_PEER_HOST")
	if host == "" {
		t.Skip("set SYNGO_BENCH_PEER_HOST to run the two-machine scenario against a real peer")
	}
	peerDir := os.Getenv("SYNGO_BENCH_PEER_DIR")
	if peerDir == "" {
		peerDir = "~/syngo-bench-remote"
	}

	dir := t.TempDir()
	cfg := runner.Config{
		Scenario:        report.ScenarioTailscaleDirect,
		Name:            "live-test",
		CorpusDir:       filepath.Join(dir, "corpus"),
		ResultsDir:      filepath.Join(dir, "results"),
		Seed:            7,
		Spec:            smallSpec(7),
		PeerHost:        host,
		PeerDir:         peerDir,
		ConvergeTimeout: 2 * time.Minute,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	outcome, err := runner.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Result.ConvergenceTime <= 0 {
		t.Errorf("convergence_time_ns = %d, want > 0", outcome.Result.ConvergenceTime)
	}
	if outcome.Result.NetworkPath == "" && !hasUnmeasured(outcome.Unmeasured, "network_path") {
		t.Error("network_path is empty and nothing explains why")
	}
	t.Logf("result %s: %+v\nunmeasured: %v", outcome.ResultPath, outcome.Result, outcome.Unmeasured)
}
