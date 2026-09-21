package jsonconfig_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := jsonconfig.Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:22000" || cfg.LogLevel != "info" {
		t.Fatalf("cfg = %+v, want defaults", cfg)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := jsonconfig.Default()
	want.DeviceName = "laptop"
	want.RescanInterval = jsonconfig.Duration(90 * time.Second)

	if err := jsonconfig.Save(dir, want); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	got, err := jsonconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got != want {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}

	raw, err := os.ReadFile(filepath.Join(dir, jsonconfig.FileName))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !strings.Contains(string(raw), `"rescan_interval": "1m30s"`) {
		t.Fatalf("config.json = %s, want a duration string", raw)
	}
}

func TestLoad_PartialFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, jsonconfig.FileName)
	if err := os.WriteFile(path, []byte(`{"device_name":"nas"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := jsonconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.DeviceName != "nas" || cfg.ChunkSize != jsonconfig.Default().ChunkSize {
		t.Fatalf("cfg = %+v, want device_name nas and default chunk size", cfg)
	}
}

func TestLoad_BadDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, jsonconfig.FileName)
	if err := os.WriteFile(path, []byte(`{"sync_interval":"soon"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := jsonconfig.Load(dir); err == nil {
		t.Fatal("Load error = nil, want a parse failure")
	}
}

// TestLoad_MissingLimitsAreUnlimited covers a config.json written before the
// limits existed: the keys are absent and the device runs unlimited.
func TestLoad_MissingLimitsAreUnlimited(t *testing.T) {
	dir := t.TempDir()
	old := `{"device_name":"laptop","listen":"127.0.0.1:22000","chunk_size":1048576}`
	if err := os.WriteFile(filepath.Join(dir, jsonconfig.FileName), []byte(old), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := jsonconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.UploadLimit != 0 || cfg.DownloadLimit != 0 {
		t.Fatalf("limits = %d/%d, want 0", cfg.UploadLimit, cfg.DownloadLimit)
	}

	settings, err := jsonconfig.NewStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("store Load error: %v", err)
	}
	if !settings.UploadLimit.Unlimited() || !settings.DownloadLimit.Unlimited() {
		t.Fatalf("settings = %+v, want unlimited limits", settings)
	}
}

func TestLoad_NegativeLimitIsAnError(t *testing.T) {
	for _, key := range []string{"upload_limit", "download_limit"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			raw := `{"` + key + `":-1}`
			if err := os.WriteFile(filepath.Join(dir, jsonconfig.FileName), []byte(raw), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := jsonconfig.Load(dir)
			if !errors.Is(err, bandwidth.ErrInvalidRate) {
				t.Fatalf("Load error = %v, want ErrInvalidRate", err)
			}
		})
	}
}

func TestStore_LoadRoundTripsLimits(t *testing.T) {
	dir := t.TempDir()
	store := jsonconfig.NewStore(dir)

	settings, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	settings.UploadLimit = 4 << 20
	settings.DownloadLimit = 1 << 20
	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if got != settings {
		t.Fatalf("Load = %+v, want %+v", got, settings)
	}
}

// TestStore_SaveKeepsDeviceName pins that the store writes through the keys
// the settings port does not carry.
func TestStore_SaveKeepsDeviceName(t *testing.T) {
	dir := t.TempDir()
	cfg := jsonconfig.Default()
	cfg.DeviceName = "laptop"
	if err := jsonconfig.Save(dir, cfg); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	store := jsonconfig.NewStore(dir)
	settings, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	settings.DownloadLimit = 2 << 20
	if err := store.Save(context.Background(), settings); err != nil {
		t.Fatalf("store Save error: %v", err)
	}

	after, err := jsonconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if after.DeviceName != "laptop" {
		t.Fatalf("device_name = %q, want it kept", after.DeviceName)
	}
	if after.DownloadLimit != 2<<20 {
		t.Fatalf("download_limit = %d, want 2097152", after.DownloadLimit)
	}
}
