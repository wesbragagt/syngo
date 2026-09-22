package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// SaveDevice inserts or updates a known peer.
func (s *Store) SaveDevice(ctx context.Context, d device.Device) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (id, name, address, verified, added_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, address = excluded.address, verified = excluded.verified,
			last_seen_at = excluded.last_seen_at`,
		string(d.ID), d.Name, d.Address, boolToInt(d.Verified), d.AddedAt.UnixNano(), nullTime(d.LastSeenAt))
	if err != nil {
		return fmt.Errorf("save device %q: %w", d.ID, err)
	}
	return nil
}

// GetDevice returns one device, or ErrNotFound.
func (s *Store) GetDevice(ctx context.Context, id device.ID) (device.Device, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, address, verified, added_at, last_seen_at FROM devices WHERE id = ?`, string(id))
	d, err := scanDevice(row)
	if err != nil {
		return device.Device{}, notFound(err, fmt.Sprintf("device %q", id))
	}
	return d, nil
}

// ListDevices returns every device ordered by id.
func (s *Store) ListDevices(ctx context.Context) ([]device.Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, address, verified, added_at, last_seen_at FROM devices ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]device.Device, 0)
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("list devices: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	return out, nil
}

// DeleteDevice removes a device.
func (s *Store) DeleteDevice(ctx context.Context, id device.ID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete device %q: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("device %q", id))
}

// TouchDevice records when a device was last seen.
func (s *Store) TouchDevice(ctx context.Context, id device.ID, seenAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`, nullTime(seenAt), string(id))
	if err != nil {
		return fmt.Errorf("touch device %q: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("device %q", id))
}

// scanDevice reads one device row.
func scanDevice(row scanner) (device.Device, error) {
	var (
		d        device.Device
		id       string
		verified int
		addedAt  int64
		lastSeen sql.NullInt64
	)
	if err := row.Scan(&id, &d.Name, &d.Address, &verified, &addedAt, &lastSeen); err != nil {
		return device.Device{}, err
	}
	d.ID = device.ID(id)
	d.Verified = verified != 0
	d.AddedAt = time.Unix(0, addedAt).UTC()
	d.LastSeenAt = timeFrom(lastSeen)
	return d, nil
}

// CounterFloor returns the highest version counter recorded for a device.
func (s *Store) CounterFloor(ctx context.Context, id device.ID) (uint64, error) {
	if id == "" {
		return 0, nil
	}
	var floor int64
	err := s.db.QueryRowContext(ctx,
		`SELECT floor FROM device_counters WHERE device_id = ?`, string(id)).Scan(&floor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read counter floor of %q: %w", id, err)
	}
	return uint64(floor), nil
}

// RaiseCounterFloor records to as the floor of a device, never lowering one.
func (s *Store) RaiseCounterFloor(ctx context.Context, id device.ID, to uint64) error {
	if id == "" || to == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO device_counters (device_id, floor) VALUES (?, ?)
		ON CONFLICT(device_id) DO UPDATE SET floor = MAX(floor, excluded.floor)`,
		string(id), int64(to))
	if err != nil {
		return fmt.Errorf("raise counter floor of %q: %w", id, err)
	}
	return nil
}
