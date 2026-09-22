// Package folder defines the synchronized folder configuration and its validation rules.
package folder

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"time"
)

// Mode controls which directions a folder synchronizes in.
type Mode string

// Folder synchronization modes.
const (
	ModeSendReceive Mode = "sendreceive"
	ModeSendOnly    Mode = "sendonly"
	ModeReceiveOnly Mode = "receiveonly"
)

// ParseMode validates s and returns it as a folder mode.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeSendReceive, ModeSendOnly, ModeReceiveOnly:
		return m, nil
	default:
		return "", fmt.Errorf("invalid folder mode %q, want %s, %s or %s",
			s, ModeSendReceive, ModeSendOnly, ModeReceiveOnly)
	}
}

// DeletePolicy controls whether remote deletions are applied locally.
type DeletePolicy string

// Delete policies.
const (
	DeletePropagate DeletePolicy = "propagate"
	DeleteNever     DeletePolicy = "never"
)

// ParseDeletePolicy validates s and returns it as a delete policy.
func ParseDeletePolicy(s string) (DeletePolicy, error) {
	switch p := DeletePolicy(s); p {
	case DeletePropagate, DeleteNever:
		return p, nil
	default:
		return "", fmt.Errorf("invalid delete policy %q, want %s or %s", s, DeletePropagate, DeleteNever)
	}
}

// SymlinkPolicy controls how symlinks encountered during a scan are treated.
type SymlinkPolicy string

// Symlink policies.
const (
	SymlinkSkip  SymlinkPolicy = "skip"
	SymlinkStore SymlinkPolicy = "store"
)

// ErrInvalidID is returned by ParseID when the input is not a folder ID.
var ErrInvalidID = errors.New("invalid folder id")

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ID identifies a folder. It matches ^[a-z0-9][a-z0-9._-]{0,63}$.
type ID string

// ParseID validates s and returns it as a folder ID.
func ParseID(s string) (ID, error) {
	if !idPattern.MatchString(s) {
		return "", fmt.Errorf("%w: %q must match [a-z0-9][a-z0-9._-]{0,63}", ErrInvalidID, s)
	}
	return ID(s), nil
}

// String returns the folder ID as a string.
func (id ID) String() string { return string(id) }

// Folder is a synchronized directory and its configuration.
type Folder struct {
	ID             ID
	Path           string
	Label          string
	Mode           Mode
	DeletePolicy   DeletePolicy
	IncludeHidden  bool
	Symlinks       SymlinkPolicy
	KeepEmptyDirs  bool
	IgnorePatterns []string
	// SyncWindows are the spans of local time this device pulls the folder
	// in. An empty list means it pulls at any time.
	SyncWindows Windows
	Paused      bool
	LastScanAt  time.Time
	LastError   string
	// LastSyncError is what the last sync cycle could not apply locally,
	// and PendingItems is how many items that was. They are the puller's
	// half of the folder's health; LastError is the scanner's half.
	LastSyncError string
	PendingItems  int
	CreatedAt     time.Time
}

// Validate reports whether the folder configuration is usable.
func (f Folder) Validate() error {
	if _, err := ParseID(string(f.ID)); err != nil {
		return err
	}
	if f.Path == "" {
		return errors.New("folder path is empty")
	}
	if !path.IsAbs(f.Path) {
		return fmt.Errorf("folder path %q is not absolute", f.Path)
	}
	if path.Clean(f.Path) != f.Path {
		return fmt.Errorf("folder path %q is not cleaned", f.Path)
	}
	switch f.Mode {
	case ModeSendReceive, ModeSendOnly, ModeReceiveOnly:
	default:
		return fmt.Errorf("invalid folder mode %q", f.Mode)
	}
	switch f.DeletePolicy {
	case DeletePropagate, DeleteNever:
	default:
		return fmt.Errorf("invalid delete policy %q", f.DeletePolicy)
	}
	switch f.Symlinks {
	case SymlinkSkip, SymlinkStore:
	default:
		return fmt.Errorf("invalid symlink policy %q", f.Symlinks)
	}
	return f.SyncWindows.Validate()
}

// InWindow reports whether the folder may pull at t. The wall-clock fields of
// t decide, in t's own location.
func (f Folder) InWindow(t time.Time) bool { return f.SyncWindows.Contains(t) }

// CanReceive reports whether the folder accepts changes from peers.
func (f Folder) CanReceive() bool { return f.Mode != ModeSendOnly }

// CanSend reports whether the folder serves its contents to peers.
func (f Folder) CanSend() bool { return f.Mode != ModeReceiveOnly }
