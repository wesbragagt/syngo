package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// GrantFolderDevice authorizes a device for a folder. Granting a device that
// is already authorized leaves the grant, and the time it was made, alone.
func (s *Store) GrantFolderDevice(ctx context.Context, id folder.ID, dev device.ID) error {
	if _, err := s.GetFolder(ctx, id); err != nil {
		return err
	}
	if _, err := s.GetDevice(ctx, dev); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO folder_devices (folder_id, device_id, granted_at) VALUES (?, ?, ?)
		ON CONFLICT(folder_id, device_id) DO NOTHING`,
		string(id), string(dev), time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("grant device %s on folder %q: %w", dev.Short(), id, err)
	}
	return nil
}

// RevokeFolderDevice withdraws the authorization of a device for a folder.
func (s *Store) RevokeFolderDevice(ctx context.Context, id folder.ID, dev device.ID) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM folder_devices WHERE folder_id = ? AND device_id = ?`, string(id), string(dev))
	if err != nil {
		return fmt.Errorf("revoke device %s on folder %q: %w", dev.Short(), id, err)
	}
	return requireRow(res, fmt.Sprintf("device %s on folder %q", dev.Short(), id))
}

// FolderAccess returns the devices authorized for a folder.
func (s *Store) FolderAccess(ctx context.Context, id folder.ID) (folder.Access, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT device_id FROM folder_devices WHERE folder_id = ? ORDER BY device_id`, string(id))
	if err != nil {
		return folder.Access{}, fmt.Errorf("list devices of folder %q: %w", id, err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return folder.Access{}, fmt.Errorf("list devices of folder %q: %w", id, err)
	}
	out := make([]device.ID, 0, len(ids))
	for _, raw := range ids {
		out = append(out, device.ID(raw))
	}
	return folder.NewAccess(out), nil
}

// DeviceFolders returns the folders a device is authorized for.
func (s *Store) DeviceFolders(ctx context.Context, dev device.ID) ([]folder.ID, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT folder_id FROM folder_devices WHERE device_id = ? ORDER BY folder_id`, string(dev))
	if err != nil {
		return nil, fmt.Errorf("list folders of device %s: %w", dev.Short(), err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("list folders of device %s: %w", dev.Short(), err)
	}
	out := make([]folder.ID, 0, len(ids))
	for _, raw := range ids {
		out = append(out, folder.ID(raw))
	}
	return out, nil
}

// scanIDs reads a one-column result of text ids and closes the rows.
func scanIDs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

var _ ports.FolderAuthStore = (*Store)(nil)
