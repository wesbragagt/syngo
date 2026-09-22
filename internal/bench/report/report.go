// Package report defines the JSON shape of one benchmark run and how to save
// and load it from the versioned results directory described in
// docs/BENCH.md.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is the current version of the Result JSON shape. Reader
// rejects any file whose Result.Schema does not match.
const SchemaVersion = 1

// Scenario names the network setup a run measured, one of the three
// described in docs/BENCH.md.
type Scenario string

const (
	ScenarioLAN             Scenario = "lan"
	ScenarioTailscaleDirect Scenario = "tailscale-direct"
	ScenarioTailscaleDERP   Scenario = "tailscale-derp"
)

// HardwareProfile is the machine a run executed on, recorded so a result can
// be traced back to what produced it.
type HardwareProfile struct {
	Hostname string `json:"hostname"`
	Arch     string `json:"arch"`
	NumCPU   int    `json:"num_cpu"`
}

// Result is one benchmark run: one scenario, one fixture, one set of
// measurements.
type Result struct {
	// Schema is the version of this struct's shape. Reader refuses a file
	// whose Schema does not equal SchemaVersion.
	Schema int `json:"schema"`

	// Name identifies the run, for example "small-files" or "deep-tree".
	Name string `json:"name"`
	// Scenario is the network setup the run measured.
	Scenario Scenario `json:"scenario"`
	// Hardware is the machine the run executed on.
	Hardware HardwareProfile `json:"hardware"`
	// StartedAt is when the run began.
	StartedAt time.Time `json:"started_at"`

	// ConvergenceTime is how long the run took for every peer to agree on
	// the final state.
	ConvergenceTime time.Duration `json:"convergence_time_ns"`
	// BytesTransferred is the total payload bytes sent over the network.
	BytesTransferred int64 `json:"bytes_transferred"`
	// CPUSeconds is the total CPU time consumed during the run.
	CPUSeconds float64 `json:"cpu_seconds"`
	// PeakMemoryBytes is the highest resident memory observed during the
	// run.
	PeakMemoryBytes int64 `json:"peak_memory_bytes"`
	// DiskWrites is the number of bytes written to disk.
	DiskWrites int64 `json:"disk_writes"`
	// RecoveryTime is how long the run took to reach convergence again
	// after an interruption; zero when the run had none.
	RecoveryTime time.Duration `json:"recovery_time_ns"`
}

// Writer saves Result values as JSON files in a versioned results directory,
// one file per run.
type Writer struct {
	// dir is the versioned results directory, for example
	// "bench/results/v1".
	dir string
}

// NewWriter returns a Writer that saves results under dir, creating it if it
// does not exist.
func NewWriter(dir string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create results directory %q: %w", dir, err)
	}
	return &Writer{dir: dir}, nil
}

// Write saves result as a JSON file named after its scenario, name and start
// time, and returns the path written.
func (w *Writer) Write(result Result) (string, error) {
	if result.Schema == 0 {
		result.Schema = SchemaVersion
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}

	filename := fmt.Sprintf("%s-%s-%s.json",
		result.StartedAt.UTC().Format("20060102T150405Z"),
		result.Scenario,
		result.Name,
	)
	path := filepath.Join(w.dir, filename)

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write result %q: %w", path, err)
	}
	return path, nil
}

// Reader loads Result values previously saved by a Writer.
type Reader struct{}

// NewReader returns a Reader.
func NewReader() *Reader {
	return &Reader{}
}

// Read loads the Result stored at path and rejects one whose schema version
// does not match SchemaVersion.
func (r *Reader) Read(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("read result %q: %w", path, err)
	}

	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, fmt.Errorf("unmarshal result %q: %w", path, err)
	}

	if result.Schema != SchemaVersion {
		return Result{}, fmt.Errorf("result %q has schema version %d, want %d", path, result.Schema, SchemaVersion)
	}

	return result, nil
}
