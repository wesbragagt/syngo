package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// SaveMergeBase records one version of a text file as a merge base candidate,
// keeping the newest ports.MergeBaseHistory of that path and dropping the
// rest. Recording the same content twice refreshes the row rather than adding
// one, so a file that is rescanned without changing does not push the version
// a divergence would need out of the history.
func (s *Store) SaveMergeBase(ctx context.Context, b ports.MergeBase) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO merge_bases (folder_id, rel_path, version, hash, content, saved_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (folder_id, rel_path, hash) DO UPDATE SET
				version = excluded.version, content = excluded.content, saved_at = excluded.saved_at`,
			string(b.FolderID), b.Path, b.Version.String(), b.Hash.String(),
			b.Content, b.SavedAt.UnixNano()); err != nil {
			return fmt.Errorf("record merge base of %q in folder %q: %w", b.Path, b.FolderID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM merge_bases
			WHERE folder_id = ? AND rel_path = ? AND hash NOT IN (
				SELECT hash FROM merge_bases WHERE folder_id = ? AND rel_path = ?
				ORDER BY saved_at DESC LIMIT ?)`,
			string(b.FolderID), b.Path, string(b.FolderID), b.Path, ports.MergeBaseHistory); err != nil {
			return fmt.Errorf("trim merge bases of %q in folder %q: %w", b.Path, b.FolderID, err)
		}
		return nil
	})
}

// MergeBases returns the candidates of a path, newest first.
func (s *Store) MergeBases(ctx context.Context, id folder.ID, path string) ([]ports.MergeBase, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version, hash, content, saved_at FROM merge_bases
		WHERE folder_id = ? AND rel_path = ? ORDER BY saved_at DESC`, string(id), path)
	if err != nil {
		return nil, fmt.Errorf("merge bases of %q in folder %q: %w", path, id, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ports.MergeBase, 0, ports.MergeBaseHistory)
	for rows.Next() {
		var version, hash string
		var content []byte
		var savedAt int64
		if err := rows.Scan(&version, &hash, &content, &savedAt); err != nil {
			return nil, fmt.Errorf("read merge base of %q in folder %q: %w", path, id, err)
		}
		parsedVersion, err := manifest.ParseVersion(version)
		if err != nil {
			// A row nothing can read is not a base: skipping it
			// costs a merge, keeping it would risk a wrong one.
			continue
		}
		parsedHash, err := manifest.ParseHash(hash)
		if err != nil {
			continue
		}
		out = append(out, ports.MergeBase{
			FolderID: id, Path: path, Version: parsedVersion, Hash: parsedHash,
			Content: content, SavedAt: time.Unix(0, savedAt).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read merge bases of %q in folder %q: %w", path, id, err)
	}
	return out, nil
}
