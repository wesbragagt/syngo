package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
)

// transferColumns is the column list shared by the transfer queries.
const transferColumns = `id, folder_id, rel_path, peer_id, hash, size, mtime_ns, mode, chunk_size,
	chunk_count, temp_path, state, error, created_at, updated_at`

// SaveTransfer inserts or updates a transfer together with its chunk rows. The
// chunk rows are written in batches rather than in one transaction: a large
// file has hundreds of thousands of them, and one transaction holding them all
// keeps the write lock for as long as it takes to build. A batch that does not
// make it is not a loss: loadChunks rebuilds the list from the chunk count, so
// a row that is missing is simply an unverified chunk.
func (s *Store) SaveTransfer(ctx context.Context, t transfer.Transfer) error {
	if err := s.saveTransferRow(ctx, t); err != nil {
		return err
	}
	for start := 0; start < len(t.Chunks); start += batchSize {
		end := min(start+batchSize, len(t.Chunks))
		batch := t.Chunks[start:end]

		err := s.tx(ctx, func(tx *sql.Tx) error {
			return saveChunks(ctx, tx, t.ID, batch)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// saveTransferRow writes the transfer itself, without its chunks.
func (s *Store) saveTransferRow(ctx context.Context, t transfer.Transfer) error {
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
		return nil
	})
}

// saveChunks writes one batch of chunk rows.
func saveChunks(ctx context.Context, tx *sql.Tx, id transfer.ID, chunks []transfer.Chunk) error {
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO transfer_chunks (transfer_id, idx, hash, verified, start_offset, byte_length)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(transfer_id, idx) DO UPDATE SET
				hash = excluded.hash, verified = excluded.verified,
				start_offset = excluded.start_offset, byte_length = excluded.byte_length`,
			string(id), c.Index, c.Hash.String(), boolToInt(c.Verified), c.Offset, c.Length); err != nil {
			return fmt.Errorf("save chunk %d of transfer %q: %w", c.Index, id, err)
		}
	}
	return nil
}

// GetTransfer returns one transfer with its chunks, or ErrNotFound.
func (s *Store) GetTransfer(ctx context.Context, id transfer.ID) (transfer.Transfer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+transferColumns+` FROM transfers WHERE id = ?`, string(id))
	t, count, err := scanTransfer(row)
	if err != nil {
		return transfer.Transfer{}, notFound(err, fmt.Sprintf("transfer %q", id))
	}
	if err := s.loadChunks(ctx, &t, count); err != nil {
		return transfer.Transfer{}, err
	}
	return t, nil
}

// FindLiveTransfer returns the unfinished transfer for a path, or ErrNotFound.
func (s *Store) FindLiveTransfer(ctx context.Context, id folder.ID, path string) (transfer.Transfer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+transferColumns+`
		FROM transfers WHERE folder_id = ? AND rel_path = ? AND state NOT IN ('done','failed')`,
		string(id), path)
	t, count, err := scanTransfer(row)
	if err != nil {
		return transfer.Transfer{}, notFound(err, fmt.Sprintf("live transfer for %q in folder %q", path, id))
	}
	if err := s.loadChunks(ctx, &t, count); err != nil {
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
	if filter.Path != "" {
		query += ` AND rel_path = ?`
		args = append(args, filter.Path)
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

	var (
		out    []transfer.Transfer
		counts []int
	)
	for rows.Next() {
		t, count, err := scanTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("list transfers: %w", err)
		}
		out = append(out, t)
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list transfers: %w", err)
	}

	for i := range out {
		if err := s.loadChunks(ctx, &out[i], counts[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MarkChunkVerified records a verified chunk and bumps the transfer timestamp.
func (s *Store) MarkChunkVerified(ctx context.Context, id transfer.ID, idx int, h manifest.Hash, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		// The index is checked against the transfer rather than against
		// the rows: a chunk whose row a torn save never wrote is
		// verified like any other, but one the transfer does not have
		// is still refused.
		var count int
		err := tx.QueryRowContext(ctx, `SELECT chunk_count FROM transfers WHERE id = ?`,
			string(id)).Scan(&count)
		if err != nil {
			return notFound(err, fmt.Sprintf("transfer %q", id))
		}
		if idx < 0 || idx >= count {
			return fmt.Errorf("chunk %d of transfer %q: %w", idx, id, ports.ErrNotFound)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO transfer_chunks (transfer_id, idx, hash, verified)
			VALUES (?, ?, ?, 1)
			ON CONFLICT(transfer_id, idx) DO UPDATE SET hash = excluded.hash, verified = 1`,
			string(id), idx, h.String()); err != nil {
			return fmt.Errorf("verify chunk %d of transfer %q: %w", idx, id, err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE transfers SET updated_at = ? WHERE id = ?`,
			now.UnixNano(), string(id))
		if err != nil {
			return fmt.Errorf("touch transfer %q: %w", id, err)
		}
		return requireRow(res, fmt.Sprintf("transfer %q", id))
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

// loadChunks fills in the chunk list of a transfer. The list has as many
// chunks as the transfer row says, and each row carries where the sender cut
// it. A row a torn save never wrote is an unverified chunk; one written before
// chunking became content-defined has no boundary of its own and is read back
// the old way, from the transfer's size and chunk size, so a download that was
// in flight during the upgrade resumes instead of starting over.
func (s *Store) loadChunks(ctx context.Context, t *transfer.Transfer, count int) error {
	chunks := make([]transfer.Chunk, count)
	for i := range chunks {
		offset := int64(i) * int64(t.ChunkSize)
		length := t.ChunkSize
		if remaining := t.Size - offset; remaining < int64(length) {
			length = int(remaining)
		}
		chunks[i] = transfer.Chunk{Index: i, Offset: offset, Length: max(length, 0)}
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT idx, hash, verified, start_offset, byte_length FROM transfer_chunks
		WHERE transfer_id = ? ORDER BY idx`, string(t.ID))
	if err != nil {
		return fmt.Errorf("load chunks of transfer %q: %w", t.ID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			idx            int
			hashHex        string
			verified       int
			offset, length sql.NullInt64
		)
		if err := rows.Scan(&idx, &hashHex, &verified, &offset, &length); err != nil {
			return fmt.Errorf("load chunks of transfer %q: %w", t.ID, err)
		}
		if idx < 0 || idx >= len(chunks) {
			// A row the chunk count does not cover belongs to a
			// transfer that was saved with another size.
			continue
		}
		hash, err := manifest.ParseHash(hashHex)
		if err != nil {
			return fmt.Errorf("chunk %d of transfer %q: %w", idx, t.ID, err)
		}
		chunks[idx].Hash = hash
		chunks[idx].Verified = verified != 0
		if offset.Valid && length.Valid {
			chunks[idx].Offset = offset.Int64
			chunks[idx].Length = int(length.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load chunks of transfer %q: %w", t.ID, err)
	}
	t.Chunks = chunks
	return nil
}

// scanTransfer reads one transfer row without its chunks.
func scanTransfer(row scanner) (transfer.Transfer, int, error) {
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
		return transfer.Transfer{}, 0, err
	}
	hash, err := manifest.ParseHash(hashHex)
	if err != nil {
		return transfer.Transfer{}, 0, fmt.Errorf("transfer %q: %w", id, err)
	}
	t.ID = transfer.ID(id)
	t.FolderID = folder.ID(folderID)
	t.PeerID = device.ID(peerID)
	t.Hash = hash
	t.Mode = uint32(mode)
	t.State = transfer.State(state)
	t.CreatedAt = time.Unix(0, createdAt).UTC()
	t.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return t, chunkCount, nil
}
