package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// transferColumns is the column list shared by the transfer queries.
const transferColumns = `id, folder_id, rel_path, peer_id, hash, size, mtime_ns, mode, chunk_size,
	chunk_count, temp_path, state, error, created_at, updated_at`

// SaveTransfer inserts or updates a transfer together with its chunk rows.
func (s *Store) SaveTransfer(ctx context.Context, t transfer.Transfer) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO transfers (`+transferColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				rel_path = excluded.rel_path, peer_id = excluded.peer_id, hash = excluded.hash,
				size = excluded.size, mtime_ns = excluded.mtime_ns, mode = excluded.mode,
				chunk_size = excluded.chunk_size, chunk_count = excluded.chunk_count,
				temp_path = excluded.temp_path, state = excluded.state, error = excluded.error,
				updated_at = excluded.updated_at`,
			string(t.ID), string(t.FolderID), t.Path, string(t.PeerID), t.Hash.String(), t.Size,
			t.MTimeNS, int64(t.Mode), t.ChunkSize, len(t.Chunks), t.TempPath, string(t.State),
			t.Error, t.CreatedAt.UnixNano(), t.UpdatedAt.UnixNano())
		if err != nil {
			return asAlreadyExists(err, fmt.Sprintf("save transfer %q", t.ID))
		}

		for _, c := range t.Chunks {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO transfer_chunks (transfer_id, idx, hash, verified)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(transfer_id, idx) DO UPDATE SET
					hash = excluded.hash, verified = excluded.verified`,
				string(t.ID), c.Index, c.Hash.String(), boolToInt(c.Verified)); err != nil {
				return fmt.Errorf("save chunk %d of transfer %q: %w", c.Index, t.ID, err)
			}
		}
		return nil
	})
}

// GetTransfer returns one transfer with its chunks, or ErrNotFound.
func (s *Store) GetTransfer(ctx context.Context, id transfer.ID) (transfer.Transfer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+transferColumns+` FROM transfers WHERE id = ?`, string(id))
	t, err := scanTransfer(row)
	if err != nil {
		return transfer.Transfer{}, notFound(err, fmt.Sprintf("transfer %q", id))
	}
	if err := s.loadChunks(ctx, &t); err != nil {
		return transfer.Transfer{}, err
	}
	return t, nil
}

// FindLiveTransfer returns the unfinished transfer for a path, or ErrNotFound.
func (s *Store) FindLiveTransfer(ctx context.Context, id folder.ID, path string) (transfer.Transfer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+transferColumns+`
		FROM transfers WHERE folder_id = ? AND rel_path = ? AND state NOT IN ('done','failed')`,
		string(id), path)
	t, err := scanTransfer(row)
	if err != nil {
		return transfer.Transfer{}, notFound(err, fmt.Sprintf("live transfer for %q in folder %q", path, id))
	}
	if err := s.loadChunks(ctx, &t); err != nil {
		return transfer.Transfer{}, err
	}
	return t, nil
}

// ListTransfers returns the transfers matching filter, ordered by id.
func (s *Store) ListTransfers(ctx context.Context, filter ports.TransferFilter) ([]transfer.Transfer, error) {
	query := `SELECT ` + transferColumns + ` FROM transfers WHERE 1 = 1`
	var args []any
	if filter.FolderID != "" {
		query += ` AND folder_id = ?`
		args = append(args, string(filter.FolderID))
	}
	if filter.State != "" {
		query += ` AND state = ?`
		args = append(args, string(filter.State))
	}
	query += ` ORDER BY id`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []transfer.Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("list transfers: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list transfers: %w", err)
	}

	for i := range out {
		if err := s.loadChunks(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MarkChunkVerified records a verified chunk and bumps the transfer timestamp.
func (s *Store) MarkChunkVerified(ctx context.Context, id transfer.ID, idx int, h manifest.Hash, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE transfer_chunks SET hash = ?, verified = 1 WHERE transfer_id = ? AND idx = ?`,
			h.String(), string(id), idx)
		if err != nil {
			return fmt.Errorf("verify chunk %d of transfer %q: %w", idx, id, err)
		}
		if err := requireRow(res, fmt.Sprintf("chunk %d of transfer %q", idx, id)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE transfers SET updated_at = ? WHERE id = ?`,
			now.UnixNano(), string(id)); err != nil {
			return fmt.Errorf("touch transfer %q: %w", id, err)
		}
		return nil
	})
}

// SetTransferState updates the state and error message of a transfer.
func (s *Store) SetTransferState(ctx context.Context, id transfer.ID, state transfer.State, errMsg string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE transfers SET state = ?, error = ?, updated_at = ? WHERE id = ?`,
		string(state), errMsg, now.UnixNano(), string(id))
	if err != nil {
		return fmt.Errorf("set state of transfer %q: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("transfer %q", id))
}

// DeleteTransfer removes a transfer and its chunks.
func (s *Store) DeleteTransfer(ctx context.Context, id transfer.ID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM transfers WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete transfer %q: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("transfer %q", id))
}

// loadChunks fills in the chunk list of a transfer. Offsets and lengths are
// derived from the size and chunk size rather than stored.
func (s *Store) loadChunks(ctx context.Context, t *transfer.Transfer) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT idx, hash, verified FROM transfer_chunks WHERE transfer_id = ? ORDER BY idx`, string(t.ID))
	if err != nil {
		return fmt.Errorf("load chunks of transfer %q: %w", t.ID, err)
	}
	defer func() { _ = rows.Close() }()

	var chunks []transfer.Chunk
	for rows.Next() {
		var (
			c        transfer.Chunk
			hashHex  string
			verified int
		)
		if err := rows.Scan(&c.Index, &hashHex, &verified); err != nil {
			return fmt.Errorf("load chunks of transfer %q: %w", t.ID, err)
		}
		hash, err := manifest.ParseHash(hashHex)
		if err != nil {
			return fmt.Errorf("chunk %d of transfer %q: %w", c.Index, t.ID, err)
		}
		c.Hash = hash
		c.Verified = verified != 0
		c.Offset = int64(c.Index) * int64(t.ChunkSize)
		c.Length = t.ChunkSize
		if remaining := t.Size - c.Offset; remaining < int64(c.Length) {
			c.Length = int(remaining)
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load chunks of transfer %q: %w", t.ID, err)
	}
	t.Chunks = chunks
	return nil
}

// scanTransfer reads one transfer row without its chunks.
func scanTransfer(row scanner) (transfer.Transfer, error) {
	var (
		t                    transfer.Transfer
		id, folderID, peerID string
		state, hashHex       string
		mode                 int64
		chunkCount           int
		createdAt, updatedAt int64
	)
	if err := row.Scan(&id, &folderID, &t.Path, &peerID, &hashHex, &t.Size, &t.MTimeNS, &mode,
		&t.ChunkSize, &chunkCount, &t.TempPath, &state, &t.Error, &createdAt, &updatedAt); err != nil {
		return transfer.Transfer{}, err
	}
	hash, err := manifest.ParseHash(hashHex)
	if err != nil {
		return transfer.Transfer{}, fmt.Errorf("transfer %q: %w", id, err)
	}
	t.ID = transfer.ID(id)
	t.FolderID = folder.ID(folderID)
	t.PeerID = device.ID(peerID)
	t.Hash = hash
	t.Mode = uint32(mode)
	t.State = transfer.State(state)
	t.CreatedAt = time.Unix(0, createdAt).UTC()
	t.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return t, nil
}
