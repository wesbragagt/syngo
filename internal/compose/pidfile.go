package compose

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// ErrDaemonRunning reports that another daemon already owns the data
// directory. Commands map it to the daemon-unavailable exit code.
var ErrDaemonRunning = errors.New("daemon already running")

// pidFileAttempts bounds the retries of the unlink race in acquirePIDFile.
const pidFileAttempts = 5

// acquirePIDFile claims path for pid by holding an advisory exclusive lock on
// it for as long as the daemon runs, and records pid in it for whoever wants
// to signal the daemon. The lock, not the number written in the file, is what
// makes the claim: the operating system drops it when the process dies however
// it died, so a pid file left behind by a daemon killed with SIGKILL is
// reclaimed, even once its pid has been handed to an unrelated process. The
// returned release function removes the pid file and drops the lock.
func acquirePIDFile(path string, pid int) (release func(), err error) {
	for attempt := 0; attempt < pidFileAttempts; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		locked, err := tryLockExclusive(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !locked {
			_ = f.Close()
			return nil, fmt.Errorf("%w%s (%s)", ErrDaemonRunning, ownerSuffix(path), path)
		}
		// A daemon shutting down removes the pid file, so the file just
		// locked can be one already unlinked, whose lock guards nothing.
		// Start over against the name, which by now is free or held by a
		// daemon that will refuse this one.
		if !sameFile(f, path) {
			_ = f.Close()
			continue
		}
		if err := writePID(f, pid); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		return func() {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				// Nothing sensible is left to do at shutdown but report it.
				fmt.Fprintf(os.Stderr, "remove %s: %v\n", path, err)
			}
			_ = f.Close() // drops the lock
		}, nil
	}
	return nil, fmt.Errorf("claim %s: it kept being replaced while being locked", path)
}

// writePID replaces the contents of f with pid.
func writePID(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0); err != nil {
		return err
	}
	return f.Sync()
}

// sameFile reports whether f is still the file path names.
func sameFile(f *os.File, path string) bool {
	locked, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(locked, named)
}

// ownerSuffix names the pid recorded in path, for the error a refused daemon
// prints. The pid is what the file says, which is only a hint: the lock is the
// claim, and the process that took it may have written nothing yet.
func ownerSuffix(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" with pid %d", pid)
}
