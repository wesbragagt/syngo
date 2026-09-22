// Package folders implements the folder configuration use cases.
package folders

import (
	"context"
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// Service adds, lists and removes synchronized folders, and says which
// devices may synchronize each of them.
type Service struct {
	store ports.FolderStore
	auth  ports.FolderAuthStore
	fs    ports.FileSystem
	clock ports.Clock
}

// New returns a folder service.
func New(store ports.FolderStore, auth ports.FolderAuthStore, fs ports.FileSystem, clock ports.Clock) *Service {
	return &Service{store: store, auth: auth, fs: fs, clock: clock}
}

// Add validates f, checks that its path is an existing directory not already
// configured, and stores it. It returns ErrAlreadyExists if either the id or
// the path is taken.
func (s *Service) Add(ctx context.Context, f folder.Folder) (folder.Folder, error) {
	if err := f.Validate(); err != nil {
		return folder.Folder{}, err
	}

	info, err := s.fs.Lstat(f.Path)
	if err != nil {
		return folder.Folder{}, fmt.Errorf("folder path %q: %w", f.Path, err)
	}
	if !info.IsDir() {
		return folder.Folder{}, fmt.Errorf("folder path %q is not a directory", f.Path)
	}

	existing, err := s.store.ListFolders(ctx)
	if err != nil {
		return folder.Folder{}, err
	}
	for _, other := range existing {
		switch {
		case other.ID == f.ID:
			return folder.Folder{}, fmt.Errorf("folder %q: %w", f.ID, ports.ErrAlreadyExists)
		case other.Path == f.Path:
			return folder.Folder{}, fmt.Errorf("folder path %q is already synchronized as %q: %w",
				f.Path, other.ID, ports.ErrAlreadyExists)
		}
	}

	if f.CreatedAt.IsZero() {
		f.CreatedAt = s.clock.Now()
	}
	if err := s.store.SaveFolder(ctx, f); err != nil {
		return folder.Folder{}, err
	}
	return f, nil
}

// List returns every configured folder, ordered by id.
func (s *Service) List(ctx context.Context) ([]folder.Folder, error) {
	return s.store.ListFolders(ctx)
}

// Get returns one folder, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id folder.ID) (folder.Folder, error) {
	return s.store.GetFolder(ctx, id)
}

// Changes are the folder settings `folder set` may change. A nil field is
// left as it is.
type Changes struct {
	Mode         *folder.Mode
	DeletePolicy *folder.DeletePolicy
	// SyncWindows replaces the whole window list. A pointer to an empty
	// list clears it, which is what makes the folder always open again.
	SyncWindows *folder.Windows
}

// Empty reports whether the change set would alter nothing.
func (c Changes) Empty() bool {
	return c.Mode == nil && c.DeletePolicy == nil && c.SyncWindows == nil
}

// Update applies c to a configured folder and stores the result. The new
// configuration is validated before it is written, so an invalid mode or
// delete policy leaves the folder untouched. It returns ErrNotFound when no
// folder has that id.
func (s *Service) Update(ctx context.Context, id folder.ID, c Changes) (folder.Folder, error) {
	f, err := s.store.GetFolder(ctx, id)
	if err != nil {
		return folder.Folder{}, err
	}
	if c.Mode != nil {
		f.Mode = *c.Mode
	}
	if c.DeletePolicy != nil {
		f.DeletePolicy = *c.DeletePolicy
	}
	if c.SyncWindows != nil {
		f.SyncWindows = *c.SyncWindows
	}
	if err := f.Validate(); err != nil {
		return folder.Folder{}, err
	}
	if err := s.store.SaveFolder(ctx, f); err != nil {
		return folder.Folder{}, err
	}
	return f, nil
}

// Remove deletes a folder from the configuration. The files on disk are left
// alone.
func (s *Service) Remove(ctx context.Context, id folder.ID) error {
	if _, err := s.store.GetFolder(ctx, id); err != nil {
		return err
	}
	return s.store.DeleteFolder(ctx, id)
}

// SetPaused pauses or resumes a folder.
func (s *Service) SetPaused(ctx context.Context, id folder.ID, paused bool) error {
	err := s.store.SetFolderPaused(ctx, id, paused)
	if errors.Is(err, ports.ErrNotFound) {
		return fmt.Errorf("folder %q: %w", id, ports.ErrNotFound)
	}
	return err
}

// Grant authorizes a device to synchronize a folder. A folder authorizes no
// device until it is granted one, so this is what makes a new folder reachable
// at all. Granting a device that is already authorized changes nothing and
// succeeds. It returns ErrNotFound when the folder or the device is unknown.
func (s *Service) Grant(ctx context.Context, id folder.ID, dev device.ID) error {
	parsed, err := device.ParseID(string(dev))
	if err != nil {
		return fmt.Errorf("%w: %s", ports.ErrNotFound, err)
	}
	return s.auth.GrantFolderDevice(ctx, id, parsed)
}

// Revoke withdraws the authorization of a device for a folder. The device
// stops being served the folder at its next request, on the connection it
// already has. It returns ErrNotFound when the device was not authorized.
func (s *Service) Revoke(ctx context.Context, id folder.ID, dev device.ID) error {
	parsed, err := device.ParseID(string(dev))
	if err != nil {
		return fmt.Errorf("%w: %s", ports.ErrNotFound, err)
	}
	return s.auth.RevokeFolderDevice(ctx, id, parsed)
}

// Access returns the devices authorized for a folder. It returns ErrNotFound
// when the folder is not configured, so the CLI does not report an empty list
// for a folder that does not exist.
func (s *Service) Access(ctx context.Context, id folder.ID) (folder.Access, error) {
	if _, err := s.store.GetFolder(ctx, id); err != nil {
		return folder.Access{}, err
	}
	return s.auth.FolderAccess(ctx, id)
}
