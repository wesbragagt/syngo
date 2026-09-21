package compose

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrDaemonRunning reports that another daemon already owns the data
// directory. Commands map it to the daemon-unavailable exit code.
var ErrDaemonRunning = errors.New("daemon already running")

// acquirePIDFile claims path for pid. A pid file left behind by a dead process
// is overwritten, together with the stale control socket. The returned release
// function removes the pid file.
func acquirePIDFile(path string, pid int) (release func(), err error) {
	if owner, ok := livePID(path); ok && owner != pid {
		return nil, fmt.Errorf("%w with pid %d (%s)", ErrDaemonRunning, owner, path)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Nothing sensible is left to do at shutdown but report it.
			fmt.Fprintf(os.Stderr, "remove %s: %v\n", path, err)
		}
	}, nil
}

// livePID returns the pid recorded in path when that process is still alive.
func livePID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, processAlive(pid)
}

// processAlive reports whether a process with that pid exists. A process owned
// by another user counts as alive: the signal is refused, not undeliverable.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	switch {
	case err == nil:
		return true
	case errors.Is(err, os.ErrPermission):
		return true
	default:
		return false
	}
}
