package compose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/fsnotifywatch"
	"github.com/wesbragagt/syncthis/internal/adapters/unixctl"
	"github.com/wesbragagt/syncthis/internal/app/daemon"
)

// Names of the files a running daemon owns inside the data directory.
const (
	ControlSocketFile = "control.sock"
	PIDFile           = "daemon.pid"
)

// ControlSocketPath returns the control socket path for a data directory.
func ControlSocketPath(dir string) string { return filepath.Join(dir, ControlSocketFile) }

// PIDFilePath returns the pid file path for a data directory.
func PIDFilePath(dir string) string { return filepath.Join(dir, PIDFile) }

// DaemonOptions overrides the persisted configuration for one daemon run.
type DaemonOptions struct {
	// Listen overrides the address reported in status; empty keeps the
	// configured one.
	Listen string
	// RescanInterval overrides how often folders are fully rescanned.
	RescanInterval time.Duration
	// Debounce overrides how long filesystem events are collected before a
	// scan; it exists for tests, the default suits real use.
	Debounce time.Duration
	// Ready, when not nil, is closed once the daemon is serving requests.
	Ready chan<- struct{}
}

// RunDaemon claims the data directory, binds the control socket and runs the
// daemon until ctx is done or a client asks it to shut down. The pid file and
// the socket are removed before it returns. It fails with ErrDaemonRunning
// when another daemon already owns the data directory.
func (a *App) RunDaemon(ctx context.Context, o DaemonOptions) error {
	if a.Scan == nil || a.Repair == nil || a.Status == nil {
		return ErrNotInitialized
	}

	// The control socket is the lock on the data directory: a daemon that
	// answers on it owns the directory, whatever the pid file says.
	server, err := listenControl(ControlSocketPath(a.DataDir), a.Logger)
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()

	release, err := acquirePIDFile(PIDFilePath(a.DataDir), os.Getpid())
	if err != nil {
		return err
	}
	defer release()

	d := daemon.New(a.daemonConfig(o), daemon.Deps{
		Store:   a.Store,
		Clock:   a.Clock,
		Watcher: fsnotifywatch.New(a.Logger),
		Scanner: a.Scan,
		Repair:  a.Repair,
		Status:  a.Status,
		Control: server,
		Log:     a.Logger,
	})
	if o.Ready != nil {
		go func() {
			select {
			case <-d.Ready():
				close(o.Ready)
			case <-ctx.Done():
			}
		}()
	}
	return d.Run(ctx)
}

// listenControl binds the control socket, replacing one left behind by a dead
// daemon. It fails with ErrDaemonRunning when a daemon still answers there.
func listenControl(path string, log *slog.Logger) (*unixctl.Server, error) {
	server, err := unixctl.Listen(path, log)
	if err == nil {
		return server, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, err
	}
	if controlAnswers(path) {
		return nil, fmt.Errorf("%w on %s", ErrDaemonRunning, path)
	}

	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("remove stale control socket %s: %w", path, err)
	}
	return unixctl.Listen(path, log)
}

// controlAnswers reports whether something accepts connections on the socket.
func controlAnswers(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// daemonConfig merges the persisted configuration with the run options.
func (a *App) daemonConfig(o DaemonOptions) daemon.Config {
	cfg := daemon.Config{
		RescanInterval: a.Config.RescanInterval.Duration(),
		Debounce:       o.Debounce,
		Listen:         a.Config.Listen,
		PID:            os.Getpid(),
	}
	if o.RescanInterval > 0 {
		cfg.RescanInterval = o.RescanInterval
	}
	if o.Listen != "" {
		cfg.Listen = o.Listen
	}
	return cfg
}
