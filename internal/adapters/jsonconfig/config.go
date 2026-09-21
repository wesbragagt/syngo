// Package jsonconfig reads and writes the config.json file in the data
// directory.
package jsonconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// FileName is the name of the config file inside the data directory.
const FileName = "config.json"

// DefaultMaxFileSize is the largest file accepted from a peer unless the
// configuration says otherwise: one tebibyte.
const DefaultMaxFileSize = 1 << 40

// Duration is a time.Duration encoded in JSON as a string such as "10m".
type Duration time.Duration

// MarshalJSON encodes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON decodes a duration string such as "10m".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Config is the persisted daemon and CLI configuration.
type Config struct {
	DeviceName string `json:"device_name"`
	Listen     string `json:"listen"`
	Interface  string `json:"interface"`
	ChunkSize  int    `json:"chunk_size"`
	// MaxFileSize is the largest file this device accepts from a peer. A
	// manifest announcing a bigger one is refused whole: the size decides
	// how many chunks are allocated and journalled, so it is not a number
	// a peer may choose freely.
	MaxFileSize    int64    `json:"max_file_size"`
	RescanInterval Duration `json:"rescan_interval"`
	SyncInterval   Duration `json:"sync_interval"`
	LogLevel       string   `json:"log_level"`
	// UploadLimit and DownloadLimit are bytes per second, 0 for
	// unlimited. They cover chunk payloads only.
	UploadLimit   int64 `json:"upload_limit"`
	DownloadLimit int64 `json:"download_limit"`
}

// Default returns the configuration used when none has been written yet.
func Default() Config {
	return Config{
		Listen:         "127.0.0.1:22000",
		ChunkSize:      transfer.DefaultChunkSize,
		MaxFileSize:    DefaultMaxFileSize,
		RescanInterval: Duration(10 * time.Minute),
		SyncInterval:   Duration(30 * time.Second),
		LogLevel:       "info",
	}
}

// Load reads config.json from dir. A missing file yields the defaults so that
// a data directory without a config still works.
func Load(dir string) (Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", FileName, err)
	}

	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", FileName, err)
	}
	return cfg, nil
}

// validate refuses a configuration the rest of the program cannot make sense
// of. A missing key is not a mistake: Load starts from the defaults.
func (c Config) validate() error {
	if c.UploadLimit < 0 {
		return fmt.Errorf("upload_limit %d: %w", c.UploadLimit, bandwidth.ErrInvalidRate)
	}
	if c.DownloadLimit < 0 {
		return fmt.Errorf("download_limit %d: %w", c.DownloadLimit, bandwidth.ErrInvalidRate)
	}
	return nil
}

// Save writes c to config.json in dir, creating the directory if needed.
func Save(dir string, c Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", FileName, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", FileName, err)
	}
	return nil
}

// Store reads and writes the settings of one data directory. It is the
// ports.SettingsStore the daemon re-reads its limits through, and the only way
// the application layer ever touches config.json.
type Store struct{ dir string }

var _ ports.SettingsStore = (*Store)(nil)

// NewStore returns a store over the config.json in dir.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Load reads the settings. A missing file yields the defaults.
func (s *Store) Load(_ context.Context) (ports.Settings, error) {
	cfg, err := Load(s.dir)
	if err != nil {
		return ports.Settings{}, err
	}
	return toSettings(cfg), nil
}

// Save writes the settings, keeping every key the port does not carry, such as
// the device name.
func (s *Store) Save(_ context.Context, set ports.Settings) error {
	cfg, err := Load(s.dir)
	if err != nil {
		return err
	}
	applySettings(&cfg, set)
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("%s: %w", FileName, err)
	}
	return Save(s.dir, cfg)
}

// toSettings is the configuration as the application sees it.
func toSettings(c Config) ports.Settings {
	return ports.Settings{
		Listen:         c.Listen,
		Interface:      c.Interface,
		ChunkSize:      c.ChunkSize,
		MaxFileSize:    c.MaxFileSize,
		RescanInterval: c.RescanInterval.Duration(),
		SyncInterval:   c.SyncInterval.Duration(),
		LogLevel:       c.LogLevel,
		UploadLimit:    bandwidth.Rate(c.UploadLimit),
		DownloadLimit:  bandwidth.Rate(c.DownloadLimit),
	}
}

// applySettings overlays s onto c, leaving the keys s does not carry alone.
func applySettings(c *Config, s ports.Settings) {
	c.Listen = s.Listen
	c.Interface = s.Interface
	c.ChunkSize = s.ChunkSize
	c.MaxFileSize = s.MaxFileSize
	c.RescanInterval = Duration(s.RescanInterval)
	c.SyncInterval = Duration(s.SyncInterval)
	c.LogLevel = s.LogLevel
	c.UploadLimit = int64(s.UploadLimit)
	c.DownloadLimit = int64(s.DownloadLimit)
}
