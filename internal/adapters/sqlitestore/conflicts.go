package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// conflictColumns are the columns every conflict query reads, in scan order.
const conflictColumns = `id, folder_id, rel_path, conflict_path, local_hash, remote_hash, peer_id,
	detected_at, resolved, reason`

// SaveConflict inserts or updates a detected conflict.
func (s *Store) SaveConflict(ctx context.Context, c ports.ConflictRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conflicts (id, folder_id, rel_path, conflict_path, local_hash, remote_hash,
		                       peer_id, detected_at, resolved, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			rel_path = excluded.rel_path, conflict_path = excluded.conflict_path,
			local_hash = excluded.local_hash, remote_hash = excluded.remote_hash,
			peer_id = excluded.peer_id, resolved = excluded.resolved,
			reason = excluded.reason`,
		c.ID, string(c.FolderID), c.Path, c.ConflictPath, c.LocalHash.String(), c.RemoteHash.String(),
		string(c.PeerID), c.DetectedAt.UnixNano(), boolToInt(c.Resolved), c.Reason)
	if err != nil {
		return fmt.Errorf("save conflict %q: %w", c.ID, err)
	}
	return nil
}

// GetConflict returns one conflict, or ports.ErrNotFound.
func (s *Store) GetConflict(ctx context.Context, id string) (ports.ConflictRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+conflictColumns+` FROM conflicts WHERE id = ?`, id)
	rec, err := scanConflict(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ConflictRecord{}, fmt.Errorf("conflict %q: %w", id, ports.ErrNotFound)
	}
	if err != nil {
		return ports.ConflictRecord{}, fmt.Errorf("get conflict %q: %w", id, err)
	}
	return rec, nil
}

// ListConflicts returns the conflicts of a folder, or of every folder when id
// is empty.
func (s *Store) ListConflicts(ctx context.Context, id folder.ID, includeResolved bool) ([]ports.ConflictRecord, error) {
	query := `SELECT ` + conflictColumns + ` FROM conflicts WHERE 1 = 1`
	var args []any
	if id != "" {
		query += ` AND folder_id = ?`
		args = append(args, string(id))
	}
	if !includeResolved {
		query += ` AND resolved = 0`
	}
	query += ` ORDER BY detected_at, id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ports.ConflictRecord
	for rows.Next() {
		rec, err := scanConflict(rows)
		if err != nil {
			return nil, fmt.Errorf("list conflicts: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	return out, nil
}

// SetConflictResolved marks a conflict as dealt with, or as open again.
func (s *Store) SetConflictResolved(ctx context.Context, id string, resolved bool) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE conflicts SET resolved = ? WHERE id = ?`, boolToInt(resolved), id)
	if err != nil {
		return fmt.Errorf("resolve conflict %q: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve conflict %q: %w", id, err)
	}
	if changed == 0 {
		return fmt.Errorf("conflict %q: %w", id, ports.ErrNotFound)
	}
	return nil
}

// scanConflict reads one conflict row.
func scanConflict(row scanner) (ports.ConflictRecord, error) {
	var (
		rec                 ports.ConflictRecord
		folderID, peerID    string
		localHex, remoteHex string
		detectedAt          int64
		resolved            int
	)
	if err := row.Scan(&rec.ID, &folderID, &rec.Path, &rec.ConflictPath, &localHex, &remoteHex,
		&peerID, &detectedAt, &resolved, &rec.Reason); err != nil {
		return ports.ConflictRecord{}, err
	}
	local, err := manifest.ParseHash(localHex)
	if err != nil {
		return ports.ConflictRecord{}, fmt.Errorf("conflict %q: %w", rec.ID, err)
	}
	remote, err := manifest.ParseHash(remoteHex)
	if err != nil {
		return ports.ConflictRecord{}, fmt.Errorf("conflict %q: %w", rec.ID, err)
	}
	rec.FolderID = folder.ID(folderID)
	rec.PeerID = device.ID(peerID)
	rec.LocalHash = local
	rec.RemoteHash = remote
	rec.DetectedAt = time.Unix(0, detectedAt).UTC()
	rec.Resolved = resolved != 0
	return rec, nil
}
