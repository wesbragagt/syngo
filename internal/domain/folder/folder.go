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

// DeletePolicy controls whether remote deletions are applied locally.
type DeletePolicy string

// Delete policies.
const (
	DeletePropagate DeletePolicy = "propagate"
	DeleteNever     DeletePolicy = "never"
)

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
	Paused         bool
	LastScanAt     time.Time
	LastError      string
	CreatedAt      time.Time
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
	return nil
}

// CanReceive reports whether the folder accepts changes from peers.
func (f Folder) CanReceive() bool { return f.Mode != ModeSendOnly }

// CanSend reports whether the folder serves its contents to peers.
func (f Folder) CanSend() bool { return f.Mode != ModeReceiveOnly }
