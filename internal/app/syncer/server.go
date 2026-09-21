// Package syncer answers what peers ask of this device. In this milestone it
// announces the identity and the folder list; manifest and chunk serving
// arrive with the sync engine.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

var _ ports.PeerHandler = (*Server)(nil)

// ErrNotImplemented reports a request whose handling arrives with the sync
// engine.
var ErrNotImplemented = errors.New("not implemented yet")

// Server is the peer-facing side of this device.
type Server struct {
	store    ports.FolderStore
	identity ports.Identity
	log      *slog.Logger
}

// New returns a server announcing identity and the folders in store.
func New(store ports.FolderStore, identity ports.Identity, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{store: store, identity: identity, log: log}
}

// Hello is what this device announces to a connecting peer. Paused folders are
// left out: the peer cannot get anything from them.
func (s *Server) Hello(ctx context.Context) ports.HelloInfo {
	info := ports.HelloInfo{
		DeviceID: s.identity.DeviceID(),
		Name:     s.identity.Name(),
		Folders:  []ports.FolderAdvert{},
	}
	configured, err := s.store.ListFolders(ctx)
	if err != nil {
		s.log.Error("listing folders for hello", "error", err)
		return info
	}
	for _, f := range configured {
		if f.Paused {
			continue
		}
		info.Folders = append(info.Folders, ports.FolderAdvert{ID: f.ID, Mode: f.Mode})
	}
	return info
}

// Manifest is implemented by the sync engine.
func (s *Server) Manifest(_ context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error) {
	return manifest.Manifest{}, fmt.Errorf("manifest of %s for %s: %w", id, peer.Short(), ErrNotImplemented)
}

// Chunk is implemented by the sync engine.
func (s *Server) Chunk(_ context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	return ports.ChunkData{}, fmt.Errorf("chunk %d of %s for %s: %w", req.Index, req.Path, peer.Short(), ErrNotImplemented)
}
