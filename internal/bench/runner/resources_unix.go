//go:build linux || darwin

package runner

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// resourceSample is a point-in-time reading of this process's resource
// counters. Run takes one before a scenario and one after, and reports the
// difference.
type resourceSample struct {
	cpuSeconds float64
	maxRSS     int64
	// diskWriteBytes is /proc/self/io's write_bytes, Linux only; -1 when it
	// could not be read, which readResourceSample turns into "not measured"
	// rather than a false zero.
	diskWriteBytes int64
}

// readResourceSample reads the current process's CPU time, peak resident
// memory and (on Linux) bytes written to disk.
func readResourceSample() resourceSample {
	var ru syscall.Rusage
	var sample resourceSample
	sample.diskWriteBytes = -1
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		sample.cpuSeconds = rusageSeconds(ru.Utime) + rusageSeconds(ru.Stime)
		sample.maxRSS = maxRSSBytes(int64(ru.Maxrss))
	}
	if written, err := readProcSelfIOWriteBytes(); err == nil {
		sample.diskWriteBytes = written
	}
	return sample
}

// rusageSeconds converts a Timeval to seconds.
func rusageSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}

// readProcSelfIOWriteBytes reads write_bytes from /proc/self/io, which only
// exists on Linux. Its absence is reported as an error, not a zero.
func readProcSelfIOWriteBytes() (int64, error) {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != "write_bytes" {
			continue
		}
		return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	}
	return 0, os.ErrNotExist
}
