package ports

import (
	"context"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
)

// Settings are the device-wide settings the application may read and write
// while it runs. They are a subset of what the configuration file holds: the
// fields that are not here, such as the device name, are none of the
// application's business and are kept by the store.
type Settings struct {
	Listen         string
	Interface      string
	ChunkSize      int
	MaxFileSize    int64
	RescanInterval time.Duration
	SyncInterval   time.Duration
	LogLevel       string
	UploadLimit    bandwidth.Rate
	DownloadLimit  bandwidth.Rate
}

// SettingsStore reads and writes the device-wide settings.
type SettingsStore interface {
	Load(ctx context.Context) (Settings, error)
	// Save writes s, keeping the fields Settings does not carry.
	Save(ctx context.Context, s Settings) error
}
