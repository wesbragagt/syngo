//go:build darwin

package runner

// maxRSSBytes returns rusage.Maxrss, which macOS already reports in bytes.
func maxRSSBytes(maxrss int64) int64 { return maxrss }
