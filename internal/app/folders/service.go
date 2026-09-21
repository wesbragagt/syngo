// Package folders implements the folder configuration use cases.
package folders

import (
	"context"
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// Service adds, lists and removes synchronized folders.
type Service struct {
	store ports.FolderStore
	fs    ports.FileSystem
	clock ports.Clock
}

// New returns a folder service.
func New(store ports.FolderStore, fs ports.FileSystem, clock ports.Clock) *Service {
	return &Service{store: store, fs: fs, clock: clock}
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
