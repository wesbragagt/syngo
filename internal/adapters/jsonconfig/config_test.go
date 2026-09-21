package jsonconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/jsonconfig"
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
