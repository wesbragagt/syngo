//go:build unix

package unixctl_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wesbragagt/syncthis/internal/adapters/unixctl"
)

// TestListen_RestrictsTheSocketToItsOwner proves the daemon cannot be
// commanded by another user of the machine, whatever the umask was.
func TestListen_RestrictsTheSocketToItsOwner(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	path := filepath.Join(t.TempDir(), "control.sock")
	server, err := unixctl.Listen(path, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = server.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != unixctl.SocketMode {
		t.Fatalf("socket mode = %v, want %v", info.Mode().Perm(), unixctl.SocketMode)
	}
}
