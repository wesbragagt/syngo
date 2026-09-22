//go:build windows

package localfs

import "io/fs"

// inodeOf returns 0 on Windows: a stat there carries no inode number, and the
// file index that stands in for one costs a second open per entry. Callers
// treat 0 as "this platform has no file identity" and fall back to the path.
func inodeOf(_ fs.FileInfo) uint64 {
	return 0
}
