//go:build unix

package compose

import (
	"errors"
	"os"
	"syscall"
)

// tryLockExclusive takes an advisory exclusive flock on f without waiting. It
// reports whether the lock was taken; an error is a real failure, not a lock
// held by somebody else.
//
// flock is used rather than a POSIX record lock because its ownership follows
// the open file description, not the process: a second daemon started inside
// the same process, which is how the integration tests start one, is refused
// as it would be from another process. It exists on every unix this project
// supports, macOS included.
func tryLockExclusive(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}
