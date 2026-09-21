package ports

import (
	"context"
	"io"
	"io/fs"
	"time"
)

// SkipDir, returned by a WalkFunc, tells Walk not to descend into a directory.
var SkipDir = fs.SkipDir

// FileInfo describes one filesystem object, without following symlinks.
type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
	Inode   uint64
}

// IsDir reports whether the entry is a directory.
func (i FileInfo) IsDir() bool { return i.Mode.IsDir() }

// IsSymlink reports whether the entry is a symbolic link.
func (i FileInfo) IsSymlink() bool { return i.Mode&fs.ModeSymlink != 0 }

// WalkFunc is called once per filesystem object visited by Walk. relPath is
// slash-separated and relative to the walk root; it is "" for the root itself.
type WalkFunc func(relPath string, info FileInfo) error

// TempFile is a partially written file that supports resumable writes.
type TempFile interface {
	io.WriterAt
	io.ReaderAt
	Truncate(size int64) error
	Sync() error
	Close() error
	Name() string
}

// FileSystem is the file access the application needs. Implementations never
// follow symlinks.
type FileSystem interface {
	Lstat(path string) (FileInfo, error)
	// Walk visits root and everything beneath it in lexical order.
	Walk(ctx context.Context, root string, fn WalkFunc) error
	Open(path string) (io.ReadCloser, error)
	// ReadAt reads up to n bytes at off. A short read at EOF is not an error.
	ReadAt(path string, off int64, n int) ([]byte, error)
	ReadLink(path string) (string, error)
	MkdirAll(path string, perm fs.FileMode) error
	CreateTemp(dir, pattern string) (TempFile, error)
	// OpenTemp reopens an existing partial file so a transfer can resume.
	OpenTemp(path string) (TempFile, error)
	Rename(oldPath, newPath string) error
	// Remove deletes a file or an empty directory.
	Remove(path string) error
	Chtimes(path string, mtime time.Time) error
	Chmod(path string, mode fs.FileMode) error
}
