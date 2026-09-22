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
	"github.com/wesbragagt/syncthis/internal/domain/chunker"
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
	ports.FolderAuthStore
	ports.FileStore
	ports.TransferStore
	ports.ChunkStore
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
	// chunkSize is the average chunk size this device cuts with. It is
	// only used for a file the chunk index does not cover.
	chunkSize int
	log       *slog.Logger
}

// New returns a server serving the folders in store. chunkSize is the average
// chunk size this device cuts with; zero takes the default.
func New(store Store, fsys ports.FileSystem, hasher ports.Hasher, identity ports.Identity,
	chunkSize int, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if chunkSize < transfer.MinChunkSize {
		chunkSize = transfer.DefaultChunkSize
	}
	return &Server{store: store, fs: fsys, hasher: hasher, identity: identity,
		chunkSize: chunkSize, log: log}
}

// SetUploadLimiter installs the limiter that paces served chunks. A nil
// limiter means unlimited. It must be called before the server starts
// answering peers; the rate itself may change at any time, through the
// limiter.
func (s *Server) SetUploadLimiter(l ports.Limiter) { s.upload = l }

// Hello is what this device announces to peer. Only the folders peer is
// authorized for are named, so a paired device never learns that the others
// exist. Paused folders are left out too: the peer cannot get anything from
// them.
func (s *Server) Hello(ctx context.Context, peer device.ID) ports.HelloInfo {
	info := ports.HelloInfo{
		DeviceID: s.identity.DeviceID(),
		Name:     s.identity.Name(),
		Folders:  []ports.FolderAdvert{},
	}
	if peer == "" {
		// Nobody has authenticated yet, so no folder is authorized.
		return info
	}
	authorized, err := s.store.DeviceFolders(ctx, peer)
	if err != nil {
		s.log.Error("listing authorized folders for hello", "peer", peer.Short(), "error", err)
		return info
	}
	for _, id := range authorized {
		f, err := s.store.GetFolder(ctx, id)
		if err != nil {
			if !errors.Is(err, ports.ErrNotFound) {
				s.log.Error("loading folder for hello", "folder", id, "error", err)
			}
			continue
		}
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
	if _, err := s.sendable(ctx, peer, id); err != nil {
		return manifest.Manifest{}, err
	}
	m, err := s.store.LoadManifest(ctx, id)
	if err != nil {
		return manifest.Manifest{}, err
	}
	s.log.Debug("served manifest", "peer", peer.Short(), "folder", id, "entries", len(m.Entries))
	return m, nil
}

// Chunks tells a peer how this device cut a file: one entry per chunk, with
// its offset, its length and its hash. The receiver needs the list before it
// can ask for anything, because boundaries are content-defined and only the
// side holding the bytes knows where they fall. It is also what lets the
// receiver recognize content it already holds and never ask for it.
//
// The list comes from the chunk index the scanner wrote. A path the index does
// not cover, which is every path of a database written before the index
// existed, is chunked here and now by reading the file, so an upgraded device
// serves its peers before it has rescanned.
func (s *Server) Chunks(ctx context.Context, peer device.ID, req ports.ChunkListRequest) ([]transfer.Chunk, error) {
	// A refusal names chunk zero: the request is about the whole file, and
	// the receiver has no index to be told about yet.
	f, entry, err := s.servable(ctx, peer, req.FolderID, req.Path, req.Hash, 0)
	if err != nil {
		return nil, err
	}
	if entry.Size == 0 {
		return nil, nil
	}

	chunks, err := s.store.FileChunks(ctx, req.FolderID, req.Path)
	switch {
	case err == nil:
		if verr := transfer.ValidateChunks(entry.Size, chunks); verr == nil {
			s.log.Debug("served chunk list", "peer", peer.Short(), "folder", req.FolderID,
				"path", req.Path, "chunks", len(chunks), "source", "index")
			return chunks, nil
		}
		// The index disagrees with the entry, which a crash between
		// two writes can leave behind. Reading the file is always
		// right, so it is what answers.
		s.log.Warn("chunk index disagrees with the file entry, re-chunking",
			"folder", req.FolderID, "path", req.Path)
	case errors.Is(err, ports.ErrNotFound):
	default:
		return nil, err
	}

	chunks, err = s.chunkFile(path.Join(f.Path, req.Path))
	if err != nil {
		return nil, err
	}
	// The file is read after the entry was checked, so a list that does not
	// describe the entry means the file moved in between.
	if err := transfer.ValidateChunks(entry.Size, chunks); err != nil {
		return nil, refuseChunk(req.FolderID, req.Path, 0, ports.ChunkHashChanged)
	}
	s.log.Debug("served chunk list", "peer", peer.Short(), "folder", req.FolderID,
		"path", req.Path, "chunks", len(chunks), "source", "file")
	return chunks, nil
}

// chunkFile cuts a file on disk and hashes every chunk.
func (s *Server) chunkFile(full string) ([]transfer.Chunk, error) {
	rc, err := s.fs.Open(full)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", full, err)
	}
	defer func() { _ = rc.Close() }()

	var chunks []transfer.Chunk
	err = chunker.Split(rc, chunker.ConfigFor(s.chunkSize), func(span chunker.Span, data []byte) error {
		chunks = append(chunks, transfer.Chunk{
			Index: span.Index, Offset: span.Offset, Length: span.Length,
			Hash: s.hasher.SumBytes(data),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", full, err)
	}
	return chunks, nil
}

// Chunk reads one chunk of a file off the local disk and hashes it. The
// request names the byte range, which the receiver took from the chunk list
// this device served: neither side derives a boundary from a size any more. A
// request this device cannot serve is refused with a *ports.ChunkError, which
// leaves the connection usable.
func (s *Server) Chunk(ctx context.Context, peer device.ID, req ports.ChunkRequest) (ports.ChunkData, error) {
	if req.Length <= 0 || req.Length > chunker.MaxSize || req.Offset < 0 {
		return ports.ChunkData{}, fmt.Errorf("chunk %d of %s: invalid request (%d bytes at %d)",
			req.Index, req.Path, req.Length, req.Offset)
	}
	f, _, err := s.servable(ctx, peer, req.FolderID, req.Path, req.Hash, req.Index)
	if err != nil {
		return ports.ChunkData{}, err
	}

	full := path.Join(f.Path, req.Path)
	data, err := s.fs.ReadAt(full, req.Offset, req.Length)
	if err != nil {
		return ports.ChunkData{}, fmt.Errorf("read %s at %d: %w", full, req.Offset, err)
	}
	if len(data) != req.Length {
		// The file is shorter than the list said, so it moved between
		// the two requests.
		return ports.ChunkData{}, refuseChunk(req.FolderID, req.Path, req.Index, ports.ChunkHashChanged)
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
		Offset:    req.Offset,
		Data:      data,
		ChunkHash: s.hasher.SumBytes(data),
	}, nil
}

// refuseChunk builds the error that refuses a chunk request without ending the
// connection.
func refuseChunk(id folder.ID, relPath string, index int, reason ports.ChunkErrorReason) error {
	return &ports.ChunkError{FolderID: id, Path: relPath, Index: index, Reason: reason}
}

// servable answers the question both chunk requests ask: may this device serve
// content of that path to peer, and is the file still the one the peer is
// downloading. It returns the folder and the stored entry.
func (s *Server) servable(ctx context.Context, peer device.ID, id folder.ID, relPath string,
	hash manifest.Hash, index int) (folder.Folder, manifest.Entry, error) {
	refuse := func(reason ports.ChunkErrorReason) error {
		s.log.Debug("refused chunk request", "peer", peer.Short(), "folder", id, "path", relPath,
			"index", index, "reason", reason)
		return refuseChunk(id, relPath, index, reason)
	}

	if err := manifest.ValidRelPath(relPath); err != nil {
		// A peer asking for a path outside the folder is answered like
		// one asking for a file that is not there.
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkNotFound)
	}
	// The authorization check comes before anything is read off the disk or
	// out of the chunk index, so a peer that is not authorized for the
	// folder learns neither a chunk boundary nor a byte of content.
	f, err := s.sendable(ctx, peer, id)
	switch {
	case errors.Is(err, ports.ErrFolderUnknown):
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkFolderUnknown)
	case errors.Is(err, ports.ErrFolderReceiveOnly):
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkFolderReceiveOnly)
	case errors.Is(err, ports.ErrPaused):
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkPaused)
	case err != nil:
		return folder.Folder{}, manifest.Entry{}, err
	}

	entry, err := s.store.GetFile(ctx, id, relPath)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkNotFound)
	case err != nil:
		return folder.Folder{}, manifest.Entry{}, err
	}
	if entry.Deleted || entry.Kind != manifest.KindFile {
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkNotFound)
	}
	if entry.Hash != hash {
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkHashChanged)
	}

	full := path.Join(f.Path, relPath)
	info, err := s.fs.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkNotFound)
	case err != nil:
		return folder.Folder{}, manifest.Entry{}, fmt.Errorf("stat %s: %w", full, err)
	}
	// The stored hash is only as fresh as the last scan, so a file whose
	// size or modification time moved is reported as changed rather than
	// served in two inconsistent halves.
	if info.Size != entry.Size || info.ModTime.UnixNano() != entry.ModTime.UnixNano() {
		return folder.Folder{}, manifest.Entry{}, refuse(ports.ChunkHashChanged)
	}
	return f, entry, nil
}

// sendable returns the folder if this device may serve its contents to peer.
//
// A folder peer is not authorized for is refused exactly as one that does not
// exist, with the same error and the same message: a peer that asks for a
// folder it was never told about must not be able to tell "no such folder"
// from "not yours". The check is made here, on every request, so a grant
// withdrawn while a peer is connected stops it at its next request rather than
// at its next handshake.
func (s *Server) sendable(ctx context.Context, peer device.ID, id folder.ID) (folder.Folder, error) {
	unknown := fmt.Errorf("folder %q: %w", id, ports.ErrFolderUnknown)

	access, err := s.store.FolderAccess(ctx, id)
	if err != nil {
		return folder.Folder{}, err
	}
	if !access.Allows(peer) {
		s.log.Debug("refused an unauthorized folder", "peer", peer.Short(), "folder", id)
		return folder.Folder{}, unknown
	}

	f, err := s.store.GetFolder(ctx, id)
	if errors.Is(err, ports.ErrNotFound) {
		return folder.Folder{}, unknown
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
