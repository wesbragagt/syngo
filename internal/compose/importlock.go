package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ImportLockFile is the file `config import` locks while it reads the
// configuration and writes the bundle into it.
const ImportLockFile = "import.lock"

// LockImport serializes the imports of one data directory. An import reads the
// configuration to find its conflicts and then writes, which is not one
// transaction across the three stores, so a second import running in between
// would find no conflict and both would report the same items as added.
//
// The lock is an advisory lock on a file in the data directory, so it is
// released by the kernel when the process dies; the file itself is left behind
// on purpose, as removing it would let another process lock a file that is
// already unlinked. The call blocks until the lock is free.
func LockImport(dir string) (release func(), err error) {
	path := filepath.Join(dir, ImportLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
