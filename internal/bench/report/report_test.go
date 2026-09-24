package report_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/bench/report"
)

func sampleResult() report.Result {
	return report.Result{
		Schema:           report.SchemaVersion,
		Name:             "small-files",
		Scenario:         report.ScenarioTailscaleDirect,
		Hardware:         report.HardwareProfile{Hostname: "bench-a", Arch: "amd64", NumCPU: 8},
		StartedAt:        time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		ConvergenceTime:  850 * time.Millisecond,
		BytesTransferred: 1024,
		CPUSeconds:       0.42,
		PeakMemoryBytes:  1 << 20,
		DiskWrites:       4096,
		RecoveryTime:     0,
	}
}

func TestWriteThenReadRoundTrips(t *testing.T) {
	tests := []struct {
		name   string
		result report.Result
	}{
		{name: "typical run", result: sampleResult()},
		{
			name: "recovery run",
			result: func() report.Result {
				r := sampleResult()
				r.Scenario = report.ScenarioTailscaleDERP
				r.RecoveryTime = 3 * time.Second
				return r
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			w, err := report.NewWriter(dir)
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}

			path, err := w.Write(tt.result)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("written file missing: %v", err)
			}
			if filepath.Dir(path) != dir {
				t.Fatalf("Write wrote to %q, want directory %q", path, dir)
			}

			r := report.NewReader()
			got, err := r.Read(path)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}

			if got != tt.result {
				t.Fatalf("round trip mismatch:\n got  %+v\n want %+v", got, tt.result)
			}
		})
	}
}

func TestWriteDefaultsMissingSchema(t *testing.T) {
	dir := t.TempDir()
	w, err := report.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	result := sampleResult()
	result.Schema = 0

	path, err := w.Write(result)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	r := report.NewReader()
	got, err := r.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Schema != report.SchemaVersion {
		t.Fatalf("Schema = %d, want %d", got.Schema, report.SchemaVersion)
	}
}

func TestReadRejectsUnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	result := sampleResult()
	result.Schema = report.SchemaVersion + 1

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "future-schema.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	r := report.NewReader()
	if _, err := r.Read(path); err == nil {
		t.Fatalf("Read did not reject unknown schema version %d", result.Schema)
	}
}

func TestReadRejectsMissingFile(t *testing.T) {
	r := report.NewReader()
	if _, err := r.Read(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatalf("Read did not reject a missing file")
	}
}
