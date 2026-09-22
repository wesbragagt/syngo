package folder

import (
	"sort"

	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// Access is the set of devices allowed to synchronize one folder.
//
// It is deny by default: the zero value and an empty set allow nobody, so a
// folder whose authorization list was never written is served to no peer. An
// authorization mistake that stops a synchronization is visible and
// reversible; one that hands a folder to a device that was never meant to see
// it is neither.
type Access struct {
	allowed map[device.ID]struct{}
}

// NewAccess returns the access set naming ids. The slice is copied, so the
// value is immutable once built.
func NewAccess(ids []device.ID) Access {
	allowed := make(map[device.ID]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		allowed[id] = struct{}{}
	}
	return Access{allowed: allowed}
}

// Allows reports whether id may read this folder: its manifest, its chunks
// and its name in a handshake. An empty device ID is never allowed.
func (a Access) Allows(id device.ID) bool {
	if id == "" {
		return false
	}
	_, ok := a.allowed[id]
	return ok
}

// Empty reports whether no device is authorized.
func (a Access) Empty() bool { return len(a.allowed) == 0 }

// Devices returns the authorized device IDs, sorted.
func (a Access) Devices() []device.ID {
	out := make([]device.ID, 0, len(a.allowed))
	for id := range a.allowed {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
