package identityfile_test

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/adapters/identityfile"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

func TestLoadOrCreate_CreatesThenLoadsSameID(t *testing.T) {
	dir := t.TempDir()

	first, created, err := identityfile.LoadOrCreate(dir, "laptop")
	if err != nil {
		t.Fatalf("LoadOrCreate error: %v", err)
	}
	if !created {
		t.Fatal("created = false on a fresh data dir")
	}
	if len(first.DeviceID()) != device.IDLength {
		t.Fatalf("device id = %q, want %d characters", first.DeviceID(), device.IDLength)
	}
	if first.Name() != "laptop" {
		t.Fatalf("Name() = %q, want %q", first.Name(), "laptop")
	}

	second, created, err := identityfile.LoadOrCreate(dir, "ignored")
	if err != nil {
		t.Fatalf("second LoadOrCreate error: %v", err)
	}
	if created {
		t.Fatal("created = true on an existing data dir")
	}
	if second.DeviceID() != first.DeviceID() || second.Name() != "laptop" {
		t.Fatalf("second load = %q/%q, want %q/%q", second.DeviceID(), second.Name(), first.DeviceID(), first.Name())
	}

	third, err := identityfile.Load(dir)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if third.DeviceID() != first.DeviceID() {
		t.Fatalf("Load id = %q, want %q", third.DeviceID(), first.DeviceID())
	}
}

func TestLoad_NotInitialized(t *testing.T) {
	if _, err := identityfile.Load(t.TempDir()); !errors.Is(err, identityfile.ErrNotInitialized) {
		t.Fatalf("Load error = %v, want ErrNotInitialized", err)
	}
}

func TestKeyFileMode0600(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := identityfile.LoadOrCreate(dir, "laptop"); err != nil {
		t.Fatalf("LoadOrCreate error: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, identityfile.KeyFile))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestIDMatchesCertSPKI(t *testing.T) {
	dir := t.TempDir()
	id, _, err := identityfile.LoadOrCreate(dir, "laptop")
	if err != nil {
		t.Fatalf("LoadOrCreate error: %v", err)
	}

	cert := id.TLSCertificate()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if want := device.IDFromPublicKey(leaf.RawSubjectPublicKeyInfo); id.DeviceID() != want {
		t.Fatalf("DeviceID() = %q, want %q", id.DeviceID(), want)
	}
	if leaf.Subject.CommonName != "syncthis" {
		t.Fatalf("CommonName = %q, want syncthis", leaf.Subject.CommonName)
	}
}
