package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/bench/fixture"
	"github.com/wesbragagt/syncthis/internal/bench/report"
	"github.com/wesbragagt/syncthis/internal/bench/runner"
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
	cfg.PeerAddr = "100.64.0.1:22000"
	if err := runner.Validate(cfg); err == nil {
		t.Fatal("Validate(lan with --peer-addr) = nil, want an error")
	}
}

func TestValidate_TailscaleNeedsPeerAddrAndDeviceIDAndLocalDataDir(t *testing.T) {
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
	cfg.PeerAddr = "100.64.0.1:22000"
	cfg.PeerDeviceID = validDeviceID(t)
	cfg.LocalDataDir = t.TempDir()
	if err := runner.Validate(cfg); err != nil {
		t.Fatalf("Validate(tailscale-direct, fully configured) = %v, want nil", err)
	}
}

func TestValidate_TailscaleRefusesBadDeviceID(t *testing.T) {
	cfg := baseConfig()
	cfg.Scenario = report.ScenarioTailscaleDirect
	cfg.PeerAddr = "100.64.0.1:22000"
	cfg.PeerDeviceID = "not-a-device-id"
	cfg.LocalDataDir = t.TempDir()
	if err := runner.Validate(cfg); err == nil {
		t.Fatal("Validate with a malformed --peer-device-id = nil, want an error")
	}
}

func TestValidate_TailscaleRefusesLANDataDirFlags(t *testing.T) {
	cfg := baseConfig()
	cfg.Scenario = report.ScenarioTailscaleDirect
	cfg.PeerAddr = "100.64.0.1:22000"
	cfg.PeerDeviceID = validDeviceID(t)
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

// validDeviceID returns a device ID that parses, without depending on a real
// identity: ParseID only checks shape.
func validDeviceID(t *testing.T) string {
	t.Helper()
	// 52 base32 characters, a shape ParseID accepts regardless of whether
	// any device actually holds this ID.
	return "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRST"
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
	if result.RecoveryTime <= 0 {
		t.Errorf("recovery_time_ns = %d, want > 0: the recovery test should have measured it", result.RecoveryTime)
	}
	if result.BytesTransferred <= 0 {
		t.Errorf("bytes_transferred = %d, want > 0", result.BytesTransferred)
	}
}
