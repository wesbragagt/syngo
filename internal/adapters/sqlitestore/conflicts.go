package sqlitestore

import (
	"context"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// SaveConflict inserts or updates a detected conflict.
func (s *Store) SaveConflict(ctx context.Context, c ports.ConflictRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conflicts (id, folder_id, rel_path, conflict_path, local_hash, remote_hash,
		                       peer_id, detected_at, resolved)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			rel_path = excluded.rel_path, conflict_path = excluded.conflict_path,
			local_hash = excluded.local_hash, remote_hash = excluded.remote_hash,
			peer_id = excluded.peer_id, resolved = excluded.resolved`,
		c.ID, string(c.FolderID), c.Path, c.ConflictPath, c.LocalHash.String(), c.RemoteHash.String(),
		string(c.PeerID), c.DetectedAt.UnixNano(), boolToInt(c.Resolved))
	if err != nil {
		return fmt.Errorf("save conflict %q: %w", c.ID, err)
	}
	return nil
}

// ListConflicts returns the conflicts of a folder, or of every folder when id
// is empty.
func (s *Store) ListConflicts(ctx context.Context, id folder.ID, includeResolved bool) ([]ports.ConflictRecord, error) {
	query := `SELECT id, folder_id, rel_path, conflict_path, local_hash, remote_hash, peer_id,
		detected_at, resolved FROM conflicts WHERE 1 = 1`
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
		var (
			rec                 ports.ConflictRecord
			folderID, peerID    string
			localHex, remoteHex string
			detectedAt          int64
			resolved            int
		)
		if err := rows.Scan(&rec.ID, &folderID, &rec.Path, &rec.ConflictPath, &localHex, &remoteHex,
			&peerID, &detectedAt, &resolved); err != nil {
			return nil, fmt.Errorf("list conflicts: %w", err)
		}
		local, err := manifest.ParseHash(localHex)
		if err != nil {
			return nil, fmt.Errorf("conflict %q: %w", rec.ID, err)
		}
		remote, err := manifest.ParseHash(remoteHex)
		if err != nil {
			return nil, fmt.Errorf("conflict %q: %w", rec.ID, err)
		}
		rec.FolderID = folder.ID(folderID)
		rec.PeerID = device.ID(peerID)
		rec.LocalHash = local
		rec.RemoteHash = remote
		rec.DetectedAt = time.Unix(0, detectedAt).UTC()
		rec.Resolved = resolved != 0
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	return out, nil
}
