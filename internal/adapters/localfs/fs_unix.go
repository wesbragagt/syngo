//go:build unix

package localfs

import (
	"io/fs"
	"syscall"
)

// inodeOf returns the inode number of a stat result, or 0 if the platform
// stat structure is unavailable.
func inodeOf(info fs.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return stat.Ino
}
