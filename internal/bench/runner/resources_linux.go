//go:build linux

package runner

// maxRSSBytes converts rusage.Maxrss, which is kibibytes on Linux, to bytes.
func maxRSSBytes(maxrss int64) int64 { return maxrss * 1024 }
