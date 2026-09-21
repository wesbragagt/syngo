// Package jsonconfig reads and writes the config.json file in the data
// directory.
package jsonconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// FileName is the name of the config file inside the data directory.
const FileName = "config.json"

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
	DeviceName     string   `json:"device_name"`
	Listen         string   `json:"listen"`
	Interface      string   `json:"interface"`
	ChunkSize      int      `json:"chunk_size"`
	RescanInterval Duration `json:"rescan_interval"`
	SyncInterval   Duration `json:"sync_interval"`
	LogLevel       string   `json:"log_level"`
}

// Default returns the configuration used when none has been written yet.
func Default() Config {
	return Config{
		Listen:         "127.0.0.1:22000",
		ChunkSize:      transfer.DefaultChunkSize,
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
	return cfg, nil
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
