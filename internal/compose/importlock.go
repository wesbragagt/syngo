package compose

import (
	"path/filepath"
)

// ImportLockFile is the file `config import` locks while it reads the
// configuration and writes the bundle into it.
const ImportLockFile = "import.lock"

// LockImport serializes the imports of one data directory. An import reads the
// configuration to find its conflicts and then writes, which is not one
// transaction across the three stores, so a second import running in between
// would find no conflict and both would report the same items as added.
//
// The lock is held on a file in the data directory, so it is released by the
// operating system when the process dies; the file itself is left behind on
// purpose, as removing it would let another process lock a file that is
// already unlinked. The call blocks until the lock is free. How the lock is
// taken is the one thing that differs per platform: an advisory flock on unix,
// an exclusive open on Windows.
func LockImport(dir string) (release func(), err error) {
	return lockExclusive(filepath.Join(dir, ImportLockFile))
}
