package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// SetFileChunks replaces the chunk index of one path with a cut of the content
// named by ix.Hash. An index with no chunks leaves the path out of the index,
// which is what a zero-byte file and a tombstone are.
func (s *Store) SetFileChunks(ctx context.Context, id folder.ID, path string, ix transfer.ChunkIndex) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return setFileChunks(ctx, tx, id, path, ix)
	})
}

// setFileChunks writes one path's chunk rows inside a transaction. Every row
// carries the whole-file hash the cut was made from, so a reader can tell an
// index of this content from one left behind by a version that happened to be
// the same length.
func setFileChunks(ctx context.Context, tx *sql.Tx, id folder.ID, path string, ix transfer.ChunkIndex) error {
	if err := deleteFileChunks(ctx, tx, id, path); err != nil {
		return err
	}
	for _, c := range ix.Chunks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO file_chunks (folder_id, rel_path, idx, start_offset, byte_length, hash, file_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(id), path, c.Index, c.Offset, c.Length, c.Hash.String(), ix.Hash.String()); err != nil {
			return fmt.Errorf("index chunk %d of %q in folder %q: %w", c.Index, path, id, err)
		}
	}
	return nil
}

// deleteFileChunks drops the chunk rows of one path.
func deleteFileChunks(ctx context.Context, tx *sql.Tx, id folder.ID, path string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM file_chunks WHERE folder_id = ? AND rel_path = ?`, string(id), path); err != nil {
		return fmt.Errorf("clear chunk index of %q in folder %q: %w", path, id, err)
	}
	return nil
}

// FileChunks returns how a file of this device was cut and the whole-file hash
// the cut was made from, or ErrNotFound when the index has nothing for that
// path. Rows written before the file hash was recorded, and rows the migration
// found no file row for, come back with a zero hash, which describes nothing.
func (s *Store) FileChunks(ctx context.Context, id folder.ID, path string) (transfer.ChunkIndex, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT idx, start_offset, byte_length, hash, file_hash FROM file_chunks
		WHERE folder_id = ? AND rel_path = ? ORDER BY idx`, string(id), path)
	if err != nil {
		return transfer.ChunkIndex{}, fmt.Errorf("chunks of %q in folder %q: %w", path, id, err)
	}
	defer func() { _ = rows.Close() }()

	var ix transfer.ChunkIndex
	for rows.Next() {
		var c transfer.Chunk
		var hashHex, fileHex string
		if err := rows.Scan(&c.Index, &c.Offset, &c.Length, &hashHex, &fileHex); err != nil {
			return transfer.ChunkIndex{}, fmt.Errorf("chunks of %q in folder %q: %w", path, id, err)
		}
		h, err := manifest.ParseHash(hashHex)
		if err != nil {
			return transfer.ChunkIndex{}, fmt.Errorf("chunk %d of %q in folder %q: %w", c.Index, path, id, err)
		}
		c.Hash = h
		if fileHex != "" {
			// Every row of one path is written by one statement, so
			// they all carry the same file hash; an unparseable one
			// stays zero and the index describes nothing.
			if fh, err := manifest.ParseHash(fileHex); err == nil {
				ix.Hash = fh
			}
		}
		ix.Chunks = append(ix.Chunks, c)
	}
	if err := rows.Err(); err != nil {
		return transfer.ChunkIndex{}, fmt.Errorf("chunks of %q in folder %q: %w", path, id, err)
	}
	if len(ix.Chunks) == 0 {
		return transfer.ChunkIndex{}, fmt.Errorf("chunks of %q in folder %q: %w", path, id, ports.ErrNotFound)
	}
	return ix, nil
}

// FindChunk returns somewhere this device holds a chunk with that hash. The
// index is only as fresh as the last scan, so the caller re-reads and re-hashes
// the bytes before using them; a row pointing at content that has changed since
// simply costs one read.
//
// A deleted path keeps no chunk rows, so what comes back always names a file
// the folder still has. Which of several copies is returned is not specified:
// they hold the same bytes by definition.
func (s *Store) FindChunk(ctx context.Context, h manifest.Hash) (ports.ChunkPlacement, error) {
	var p ports.ChunkPlacement
	var folderID string
	err := s.db.QueryRowContext(ctx, `
		SELECT folder_id, rel_path, start_offset, byte_length FROM file_chunks
		WHERE hash = ? LIMIT 1`, h.String()).Scan(&folderID, &p.Path, &p.Offset, &p.Length)
	if err != nil {
		return ports.ChunkPlacement{}, notFound(err, fmt.Sprintf("chunk %s", h.String()))
	}
	p.FolderID = folder.ID(folderID)
	return p, nil
}

// IndexedPaths returns the paths of a folder the chunk index covers.
func (s *Store) IndexedPaths(ctx context.Context, id folder.ID) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT rel_path FROM file_chunks WHERE folder_id = ?`, string(id))
	if err != nil {
		return nil, fmt.Errorf("indexed paths of folder %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]struct{})
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("indexed paths of folder %q: %w", id, err)
		}
		out[path] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("indexed paths of folder %q: %w", id, err)
	}
	return out, nil
}
