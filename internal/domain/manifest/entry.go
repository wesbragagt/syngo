package manifest

import (
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// Kind distinguishes the filesystem object an Entry describes.
type Kind uint8

// Entry kinds. The numeric values are persisted, so they must not change.
const (
	KindFile    Kind = 1
	KindDir     Kind = 2
	KindSymlink Kind = 3
)

// String returns the lowercase name of the kind.
func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindDir:
		return "dir"
	case KindSymlink:
		return "symlink"
	default:
		return "unknown"
	}
}

// Entry is one file, directory or symlink inside a folder. Path is relative to
// the folder root and always slash-separated.
type Entry struct {
	Path       string
	Kind       Kind
	Size       int64
	ModTime    time.Time
	Mode       uint32
	Hash       Hash
	LinkTarget string
	Inode      uint64
	Deleted    bool
	ModifiedBy device.ID
	Sequence   uint64
	SyncedHash Hash
}
