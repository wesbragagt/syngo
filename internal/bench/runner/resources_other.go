//go:build !linux && !darwin

package runner

// resourceSample is a point-in-time reading of this process's resource
// counters, left zero on platforms without a portable way to read them.
type resourceSample struct {
	cpuSeconds     float64
	maxRSS         int64
	diskWriteBytes int64
}

// readResourceSample reports nothing on this platform: CPU seconds, peak
// memory and disk writes have no portable stdlib source outside Linux and
// Darwin, and this package adds no third-party dependency to get one.
func readResourceSample() resourceSample {
	return resourceSample{diskWriteBytes: -1}
}
