// Package localfs provides a ports.FileSystem backed by the local operating
// system, never following symbolic links.
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

var _ ports.FileSystem = FS{}

// FS is the local filesystem.
type FS struct{}

// New returns a local filesystem.
func New() FS { return FS{} }

// Lstat describes the object at path without following symlinks. Like every
// method here, it returns the operating system error unchanged: an *os.PathError
// already names the operation and the path.
func (FS) Lstat(path string) (ports.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ports.FileInfo{}, err
	}
	return toFileInfo(info), nil
}

// Walk visits root and everything beneath it in lexical order. The walk
// function receives slash-separated paths relative to root; the root itself is
// reported as "".
func (FS) Walk(ctx context.Context, root string, fn ports.WalkFunc) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		return fn(filepath.ToSlash(rel), toFileInfo(info))
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}
	return nil
}

// Open opens a file for reading.
func (FS) Open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ReadAt reads up to n bytes at off. A short read at end of file is not an
// error.
func (FS) ReadAt(path string, off int64, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, n)
	read, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("at offset %d: %w", off, err)
	}
	return buf[:read], nil
}

// ReadLink returns the target of a symbolic link.
func (FS) ReadLink(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	return target, nil
}

// Symlink creates a symbolic link at path pointing at target.
func (FS) Symlink(target, path string) error {
	if err := os.Symlink(target, path); err != nil {
		return err
	}
	return nil
}

// MkdirAll creates a directory and any missing parents.
func (FS) MkdirAll(path string, perm fs.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	return nil
}

// CreateTemp creates a new temporary file in dir.
func (FS) CreateTemp(dir, pattern string) (ports.TempFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// OpenTemp reopens an existing partial file for reading and writing.
func (FS) OpenTemp(path string) (ports.TempFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Rename moves a file or directory.
func (FS) Rename(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	return nil
}

// SyncDir flushes a directory entry to disk, so a rename into it survives a
// power loss. A directory that cannot be opened for reading is reported, but a
// platform that refuses to fsync a directory is not: the rename is durable
// there by other means.
func (FS) SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// Remove deletes a file or an empty directory.
func (FS) Remove(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
}

// Chtimes sets the modification time, leaving the access time as it is.
func (FS) Chtimes(path string, mtime time.Time) error {
	if err := os.Chtimes(path, time.Time{}, mtime); err != nil {
		return err
	}
	return nil
}

// Chmod sets the permission bits of a file.
func (FS) Chmod(path string, mode fs.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return nil
}

// toFileInfo converts an fs.FileInfo into the port's representation.
func toFileInfo(info fs.FileInfo) ports.FileInfo {
	return ports.FileInfo{
		Name:    info.Name(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    info.Mode(),
		Inode:   inodeOf(info),
	}
}
