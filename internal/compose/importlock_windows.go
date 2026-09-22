//go:build windows

package compose

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// errSharingViolation is ERROR_SHARING_VIOLATION, what CreateFile returns
// while another process holds the file open with no sharing. The syscall
// package does not name it.
const errSharingViolation = syscall.Errno(32)

// lockRetryInterval is how long lockExclusive waits before trying the open
// again while another process holds the lock.
const lockRetryInterval = 50 * time.Millisecond

// lockExclusive opens path with no sharing, which Windows refuses while
// another process holds it open the same way. That is the lock: the operating
// system drops it when the handle closes, the process exiting included. The
// open is retried until it succeeds, so the call blocks like the unix flock it
// stands in for.
func lockExclusive(path string) (release func(), err error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	for {
		handle, err := syscall.CreateFile(
			name,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0, // no sharing: this is the lock
			nil,
			syscall.OPEN_ALWAYS,
			syscall.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			f := os.NewFile(uintptr(handle), path)
			return func() { _ = f.Close() }, nil
		}
		if err != errSharingViolation && err != syscall.ERROR_ACCESS_DENIED {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		time.Sleep(lockRetryInterval)
	}
}
