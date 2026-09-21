// Package configbundle turns the configuration of a device into one JSON
// document and back: the settings, the folders and the known peers. Identity
// material is never part of it.
package configbundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// SchemaVersion is the version of the bundle document. A bundle of another
// version is refused rather than guessed at.
const SchemaVersion = 1

// ErrInvalidBundle reports a document that cannot be read or does not describe
// a usable configuration.
var ErrInvalidBundle = errors.New("invalid configuration bundle")

// Bundle is one device's configuration, without anything that identifies the
// device: the device name is baked into its certificate, and the key pair, the
// device ID and the database contents are never exported.
type Bundle struct {
	SchemaVersion int      `json:"schema_version"`
	ExportedAt    string   `json:"exported_at"`
	Settings      Settings `json:"settings"`
	Folders       []Folder `json:"folders"`
	Peers         []Peer   `json:"peers"`
}

// Settings are the device-wide settings, as `config show` prints them.
type Settings struct {
	Listen         string `json:"listen"`
	Interface      string `json:"interface"`
	ChunkSize      int    `json:"chunk_size"`
	MaxFileSize    int64  `json:"max_file_size"`
	RescanInterval string `json:"rescan_interval"`
	SyncInterval   string `json:"sync_interval"`
	LogLevel       string `json:"log_level"`
	UploadLimit    int64  `json:"upload_limit"`
	DownloadLimit  int64  `json:"download_limit"`
}

// Folder is one synchronized folder, in the shape `folder add` prints.
type Folder struct {
	ID             string   `json:"id"`
	Path           string   `json:"path"`
	Label          string   `json:"label"`
	Mode           string   `json:"mode"`
	DeletePolicy   string   `json:"delete_policy"`
	Symlinks       string   `json:"symlinks"`
	IncludeHidden  bool     `json:"include_hidden"`
	KeepEmptyDirs  bool     `json:"keep_empty_dirs"`
	IgnorePatterns []string `json:"ignore_patterns"`
	SyncWindows    []string `json:"sync_windows"`
	Paused         bool     `json:"paused"`
}

// Peer is one known peer. The times it was added and last seen belong to this
// device's history, not to its configuration, so they are left out.
type Peer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Verified bool   `json:"verified"`
}

// Decode reads a bundle. Unknown fields are refused rather than ignored, so a
// typo in a hand-written document is reported instead of dropped.
func Decode(r io.Reader) (Bundle, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return Bundle{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	if b.SchemaVersion != SchemaVersion {
		return Bundle{}, fmt.Errorf("%w: schema_version %d, want %d",
			ErrInvalidBundle, b.SchemaVersion, SchemaVersion)
	}
	return b, nil
}

// Encode writes a bundle as indented JSON with a trailing newline.
func Encode(w io.Writer, b Bundle) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		return fmt.Errorf("write configuration bundle: %w", err)
	}
	return nil
}
