// Package syncer moves file contents between devices. Server answers what
// peers ask of this device, Puller fetches what a peer has and this device
// lacks, and Installer puts a finished download in place atomically.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

var _ ports.PeerHandler = (*Server)(nil)

// Store is the persistence the sync engine needs. ports.MetadataStore
// satisfies it.
type Store interface {
	ports.FolderStore
	ports.FileStore
	ports.TransferStore
}

// Server is the peer-facing side of this device: it announces the identity,
// serves folder manifests and reads chunks off the local disk.
type Server struct {
	store    Store
	fs       ports.FileSystem
	hasher   ports.Hasher
	identity ports.Identity
	// upload paces the bytes this device serves. A nil limiter means
	// unlimited. It is installed by the composition root before the server
	// answers anything.
	upload ports.Limiter
	log    *slog.Logger
}

// New returns a server serving the folders in store.
func New(store Store, fsys ports.FileSystem, hasher ports.Hasher, identity ports.Identity, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{store: store, fs: fsys, hasher: hasher, identity: identity, log: log}
}

// SetUploadLimiter installs the limiter that paces served chunks. A nil
// limiter means unlimited. It must be called before the server starts
// answering peers; the rate itself may change at any time, through the
// limiter.
func (s *Server) SetUploadLimiter(l ports.Limiter) { s.upload = l }

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

// Manifest returns everything this device knows about a folder, tombstones
// included, so the peer can decide what to pull and what to delete. It fails
// with ports.ErrFolderUnknown, ports.ErrFolderReceiveOnly or ports.ErrPaused.
func (s *Server) Manifest(ctx context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error) {
	if _, err := s.sendable(ctx, id); err != nil {
		return manifest.Manifest{}, err
	}
	m, err := s.store.LoadManifest(ctx, id)
	if err != nil {
		return manifest.Manifest{}, err
	}
	s.log.Debug("served manifest", "peer", peer.Short(), "folder", id, "entries", len(m.Entries))
	return m, nil
}

// Chunk reads one chunk of a file off the local disk and hashes it. A request
// this device cannot serve is refused with a *ports.ChunkError, which leaves
// the connection usable.
func (s *Server) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	refuse := func(reason ports.ChunkErrorReason) (ports.ChunkData, error) {
		s.log.Debug("refused chunk", "peer", peer.Short(), "folder", req.FolderID,
			"path", req.Path, "index", req.Index, "reason", reason)
		return ports.ChunkData{}, &ports.ChunkError{
			FolderID: req.FolderID, Path: req.Path, Index: req.Index, Reason: reason,
		}
	}

	if err := manifest.ValidRelPath(req.Path); err != nil {
		// A peer asking for a path outside the folder is answered like
		// one asking for a file that is not there.
		return refuse(ports.ChunkNotFound)
	}
	if req.ChunkSize < transfer.MinChunkSize || req.Index < 0 {
		return ports.ChunkData{}, fmt.Errorf("chunk %d of %s: invalid request (chunk size %d)",
			req.Index, req.Path, req.ChunkSize)
	}
	f, err := s.sendable(ctx, req.FolderID)
	switch {
	case errors.Is(err, ports.ErrFolderUnknown):
		return refuse(ports.ChunkFolderUnknown)
	case errors.Is(err, ports.ErrFolderReceiveOnly):
		return refuse(ports.ChunkFolderReceiveOnly)
	case errors.Is(err, ports.ErrPaused):
		return refuse(ports.ChunkPaused)
	case err != nil:
		return ports.ChunkData{}, err
	}

	entry, err := s.store.GetFile(ctx, req.FolderID, req.Path)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return refuse(ports.ChunkNotFound)
	case err != nil:
		return ports.ChunkData{}, err
	}
	if entry.Deleted || entry.Kind != manifest.KindFile {
		return refuse(ports.ChunkNotFound)
	}
	if entry.Hash != req.Hash {
		return refuse(ports.ChunkHashChanged)
	}

	full := path.Join(f.Path, req.Path)
	info, err := s.fs.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(ports.ChunkNotFound)
	case err != nil:
		return ports.ChunkData{}, fmt.Errorf("stat %s: %w", full, err)
	}
	// The stored hash is only as fresh as the last scan, so a file whose
	// size or modification time moved is reported as changed rather than
	// served in two inconsistent halves.
	if info.Size != entry.Size || info.ModTime.UnixNano() != entry.ModTime.UnixNano() {
		return refuse(ports.ChunkHashChanged)
	}

	offset := int64(req.Index) * int64(req.ChunkSize)
	if offset >= info.Size {
		return refuse(ports.ChunkNotFound)
	}
	length := int64(req.ChunkSize)
	if remaining := info.Size - offset; remaining < length {
		length = remaining
	}

	data, err := s.fs.ReadAt(full, offset, int(length))
	if err != nil {
		return ports.ChunkData{}, fmt.Errorf("read %s at %d: %w", full, offset, err)
	}
	if int64(len(data)) != length {
		return refuse(ports.ChunkHashChanged)
	}
	// The bytes are paced after they are read and before they are handed
	// back, so a peer held at the limit occupies its own connection
	// goroutine and nothing else.
	if s.upload != nil {
		if err := s.upload.Wait(ctx, len(data)); err != nil {
			return ports.ChunkData{}, err
		}
	}
	return ports.ChunkData{
		Index:     req.Index,
		Offset:    offset,
		Data:      data,
		ChunkHash: s.hasher.SumBytes(data),
	}, nil
}

// sendable returns the folder if this device may serve its contents.
func (s *Server) sendable(ctx context.Context, id folder.ID) (folder.Folder, error) {
	f, err := s.store.GetFolder(ctx, id)
	if errors.Is(err, ports.ErrNotFound) {
		return folder.Folder{}, fmt.Errorf("folder %q: %w", id, ports.ErrFolderUnknown)
	}
	if err != nil {
		return folder.Folder{}, err
	}
	switch {
	case !f.CanSend():
		return folder.Folder{}, fmt.Errorf("folder %q: %w", id, ports.ErrFolderReceiveOnly)
	case f.Paused:
		return folder.Folder{}, fmt.Errorf("folder %q: %w", id, ports.ErrPaused)
	}
	return f, nil
}
