package ports

import (
	"context"
	"slices"
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

// LogLevels are the levels the daemon can be started with, from the most to
// the least verbose. Every path that writes a level, `config set` as well as
// an imported bundle, checks a value against this list.
var LogLevels = []string{"debug", "info", "warn", "error"}

// ValidLogLevel reports whether level is one of LogLevels.
func ValidLogLevel(level string) bool { return slices.Contains(LogLevels, level) }

// ValidateListen reports whether a listen address is one the daemon could
// bind. It is a port because the check belongs to the network adapter, and
// both `config set` and `config import` have to make the same one.
type ValidateListen func(listen string) error
