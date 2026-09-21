package ports

import "context"

// WatchOp is the kind of filesystem change reported by a Watcher.
type WatchOp uint8

// Watch operations.
const (
	OpCreate WatchOp = iota + 1
	OpWrite
	OpRemove
	OpRename
	OpChmod
	// OpOverflow means events were dropped and a full rescan is needed.
	OpOverflow
)

// String returns the lowercase name of the operation.
func (o WatchOp) String() string {
	switch o {
	case OpCreate:
		return "create"
	case OpWrite:
		return "write"
	case OpRemove:
		return "remove"
	case OpRename:
		return "rename"
	case OpChmod:
		return "chmod"
	case OpOverflow:
		return "overflow"
	default:
		return "unknown"
	}
}

// WatchEvent is a single filesystem change. Path is absolute.
type WatchEvent struct {
	Path string
	Op   WatchOp
}

// Watcher reports changes beneath a directory tree.
type Watcher interface {
	// Watch sends events for root and its subdirectories, including
	// directories created after the watch starts, until ctx is done.
	Watch(ctx context.Context, root string, out chan<- WatchEvent) error
}
