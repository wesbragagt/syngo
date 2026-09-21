// Package syncplan decides, for a folder, which files must be pulled, deleted
// or left alone after comparing the local and remote manifests.
//
// Milestone 1 defines the vocabulary only; the comparison rules land in
// milestone 6.
package syncplan

import "github.com/wesbragagt/syncthis/internal/domain/manifest"

// Action is what a sync plan says to do with one path.
type Action uint8

// Plan actions.
const (
	// ActionNone means local and remote agree.
	ActionNone Action = iota
	// ActionPull means the remote version must be downloaded.
	ActionPull
	// ActionDelete means the local version must be removed.
	ActionDelete
	// ActionConflict means both sides changed and the versions differ.
	ActionConflict
)

// String returns the lowercase name of the action.
func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionPull:
		return "pull"
	case ActionDelete:
		return "delete"
	case ActionConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// Item is one decision in a plan, describing what to do with Path.
type Item struct {
	Action Action
	Path   string
	Local  *manifest.Entry
	Remote *manifest.Entry
}
