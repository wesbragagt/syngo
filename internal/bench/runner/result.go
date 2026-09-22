package runner

import (
	"fmt"
	"os"
	"runtime"

	"github.com/wesbragagt/syncthis/internal/bench/report"
)

// hardwareProfile records the machine a run executed on, per
// docs/BENCH.md#reference-hardware.
func hardwareProfile() report.HardwareProfile {
	hostname, _ := os.Hostname()
	return report.HardwareProfile{
		Hostname: hostname,
		Arch:     runtime.GOARCH,
		NumCPU:   runtime.NumCPU(),
	}
}

// applyResourceDelta fills result's CPU, memory and disk fields from two
// resourceSample readings, and records in unmeasured whatever a platform
// could not answer, rather than leaving a silent zero.
func applyResourceDelta(result *report.Result, before, after resourceSample, unmeasured *Unmeasured) {
	result.CPUSeconds = after.cpuSeconds - before.cpuSeconds
	if after.cpuSeconds == 0 && before.cpuSeconds == 0 {
		unmeasured.note("cpu_seconds", "no rusage support on this platform")
	}

	result.PeakMemoryBytes = after.maxRSS
	if after.maxRSS == 0 {
		unmeasured.note("peak_memory_bytes", "no rusage support on this platform")
	}

	if before.diskWriteBytes < 0 || after.diskWriteBytes < 0 {
		unmeasured.note("disk_writes", "/proc/self/io is not available on this platform")
	} else {
		result.DiskWrites = after.diskWriteBytes - before.diskWriteBytes
	}
}

// writeResult saves result to cfg.ResultsDir and returns the Outcome.
func writeResult(cfg Config, result report.Result, unmeasured Unmeasured) (Outcome, error) {
	writer, err := report.NewWriter(cfg.ResultsDir)
	if err != nil {
		return Outcome{}, err
	}
	path, err := writer.Write(result)
	if err != nil {
		return Outcome{}, fmt.Errorf("write result: %w", err)
	}
	return Outcome{Result: result, ResultPath: path, Unmeasured: unmeasured}, nil
}
