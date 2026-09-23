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
	// Version orders this entry against the same path on a peer: the
	// scanner raises this device's counter on every content change, an
	// install adopts the peer's vector, and a conflict outcome adopts the
	// merge of both. It makes Entry non-comparable, so entries are compared
	// field by field, never with ==.
	Version Version
	// SyncedMode is the permission bits the local and the remote side last
	// agreed on. It is local bookkeeping, never sent to a peer: a local mode
	// that differs from it is a local change, which the peer must not undo.
	SyncedMode uint32
	// Provisional reports that Version was handed out from a counter floor
	// this device had not yet confirmed against any peer, which is what
	// every version written between a restore from a backup and the first
	// contact looks like. Such a counter may be one a peer already holds
	// for this device, so a peer version that appears to descend from it
	// may in fact have been written independently. A provisional entry the
	// peer's version dominates is therefore kept as a conflict instead of
	// being overwritten; see syncplan.
	//
	// It is local bookkeeping, never sent to a peer: only the device that
	// handed the counter out knows whether its floor was confirmed, and
	// only that device has to act on it.
	Provisional bool
}
