package compose

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvDataDir overrides the default data directory location.
const EnvDataDir = "SYNCTHIS_DATA_DIR"

// ResolveDataDir returns the data directory to use: the flag value if set,
// else $SYNCTHIS_DATA_DIR, else $XDG_DATA_HOME/syncthis, else
// ~/.local/share/syncthis. The result is absolute and cleaned.
func ResolveDataDir(flag string) (string, error) {
	candidate := flag
	if candidate == "" {
		candidate = os.Getenv(EnvDataDir)
	}
	if candidate == "" {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			candidate = filepath.Join(xdg, "syncthis")
		}
	}
	if candidate == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		candidate = filepath.Join(home, ".local", "share", "syncthis")
	}

	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve data dir %q: %w", candidate, err)
	}
	return abs, nil
}
