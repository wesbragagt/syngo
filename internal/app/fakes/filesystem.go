package fakes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// node is one in-memory filesystem object.
type node struct {
	mode    fs.FileMode
	data    []byte
	modTime time.Time
	inode   uint64
	target  string
}

// FileSystem is an in-memory ports.FileSystem. Paths are slash-separated and
// absolute. It records renames and removals so tests can assert on them.
type FileSystem struct {
	mu      sync.Mutex
	nodes   map[string]*node
	nextIno uint64
	tempSeq int
	renames [][2]string
	removes []string
	// syncedDirs records the directories SyncDir flushed.
	syncedDirs []string
	modTime    time.Time
	// renameFailures are destinations the next Rename refuses, so a test
	// can see what a caller does when an install cannot be completed.
	renameFailures map[string]error
	// readOnly are the subtrees no write reaches, which is a mount a
	// device may read and not write.
	readOnly []string
}

// NewFileSystem returns an empty filesystem containing only "/".
func NewFileSystem() *FileSystem {
	fsys := &FileSystem{
		nodes:          make(map[string]*node),
		nextIno:        1,
		renameFailures: make(map[string]error),
		modTime:        time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC),
	}
	fsys.nodes["/"] = &node{mode: fs.ModeDir | 0o755, modTime: fsys.modTime, inode: fsys.newInode()}
	return fsys
}

// MakeReadOnly refuses every write to root and everything under it, the way
// a read-only mount does. Reads are unaffected.
func (f *FileSystem) MakeReadOnly(root string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readOnly = append(f.readOnly, clean(root))
}

// refuse reports the error a write to p owes to a read-only subtree, or nil
// when p may be written. The caller must hold f.mu.
func (f *FileSystem) refuse(op, p string) error {
	p = clean(p)
	for _, root := range f.readOnly {
		if p == root || strings.HasPrefix(p, childPrefix(root)) {
			return &fs.PathError{Op: op, Path: p, Err: errors.New("read-only file system")}
		}
	}
	return nil
}

// newInode returns the next inode number. The caller need not hold the lock
// before the filesystem is shared.
func (f *FileSystem) newInode() uint64 {
	n := f.nextIno
	f.nextIno++
	return n
}

// AddDir creates a directory and any missing parents.
func (f *FileSystem) AddDir(p string, perm fs.FileMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirAll(clean(p), perm)
}

// AddFile creates a file with the given contents, creating parent directories.
func (f *FileSystem) AddFile(p string, data []byte, perm fs.FileMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	f.mkdirAll(path.Dir(p), 0o755)
	f.nodes[p] = &node{mode: perm.Perm(), data: append([]byte(nil), data...), modTime: f.modTime, inode: f.newInode()}
}

// AddSymlink creates a symlink pointing at target.
func (f *FileSystem) AddSymlink(p, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	f.mkdirAll(path.Dir(p), 0o755)
	f.nodes[p] = &node{mode: fs.ModeSymlink | 0o777, target: target, modTime: f.modTime, inode: f.newInode()}
}

// FailNextRenameTo makes the next Rename with p as its destination fail with
// err. Later ones succeed, so a test can see both what a caller does when an
// install cannot be completed and that it recovers.
func (f *FileSystem) FailNextRenameTo(p string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renameFailures[clean(p)] = err
}

// Renames returns the (old, new) pairs passed to Rename, in order.
func (f *FileSystem) Renames() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.renames...)
}

// Removes returns the paths passed to Remove, in order.
func (f *FileSystem) Removes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removes...)
}

// Content returns the bytes stored at p.
func (f *FileSystem) Content(p string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[clean(p)]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), n.data...), true
}

// Paths returns every path in the filesystem, sorted.
func (f *FileSystem) Paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.nodes))
	for p := range f.nodes {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Lstat returns information about p without following symlinks.
func (f *FileSystem) Lstat(p string) (ports.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	n, ok := f.nodes[p]
	if !ok {
		return ports.FileInfo{}, notExist("lstat", p)
	}
	return f.infoLocked(p, n), nil
}

// Walk visits root and everything beneath it in lexical order.
func (f *FileSystem) Walk(ctx context.Context, root string, fn ports.WalkFunc) error {
	root = clean(root)

	f.mu.Lock()
	if _, ok := f.nodes[root]; !ok {
		f.mu.Unlock()
		return notExist("walk", root)
	}
	paths := make([]string, 0, len(f.nodes))
	for p := range f.nodes {
		if p == root || strings.HasPrefix(p, childPrefix(root)) {
			paths = append(paths, p)
		}
	}
	f.mu.Unlock()
	sort.Strings(paths)

	var skipped []string
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if underAny(p, skipped) {
			continue
		}

		f.mu.Lock()
		n, ok := f.nodes[p]
		if !ok {
			f.mu.Unlock()
			continue
		}
		info := f.infoLocked(p, n)
		f.mu.Unlock()

		rel := relTo(root, p)
		err := fn(rel, info)
		switch {
		case errors.Is(err, ports.SkipDir):
			skipped = append(skipped, p)
		case err != nil:
			return err
		}
	}
	return nil
}

// Open returns a reader over the contents of p.
func (f *FileSystem) Open(p string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[clean(p)]
	if !ok {
		return nil, notExist("open", p)
	}
	if n.mode.IsDir() {
		return nil, fmt.Errorf("open %s: is a directory", p)
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), n.data...))), nil
}

// ReadAt reads up to n bytes at off. A short read at EOF is not an error.
func (f *FileSystem) ReadAt(p string, off int64, n int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nd, ok := f.nodes[clean(p)]
	if !ok {
		return nil, notExist("readat", p)
	}
	if off < 0 || off > int64(len(nd.data)) {
		return nil, fmt.Errorf("readat %s: offset %d out of range", p, off)
	}
	end := off + int64(n)
	if end > int64(len(nd.data)) {
		end = int64(len(nd.data))
	}
	return append([]byte(nil), nd.data[off:end]...), nil
}

// ReadLink returns the target of a symlink.
func (f *FileSystem) ReadLink(p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[clean(p)]
	if !ok {
		return "", notExist("readlink", p)
	}
	if n.mode&fs.ModeSymlink == 0 {
		return "", fmt.Errorf("readlink %s: not a symlink", p)
	}
	return n.target, nil
}

// Symlink creates a symbolic link at p pointing at target.
func (f *FileSystem) Symlink(target, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	if _, ok := f.nodes[p]; ok {
		return fmt.Errorf("symlink %s: %w", p, fs.ErrExist)
	}
	if err := f.refuse("symlink", p); err != nil {
		return err
	}
	f.mkdirAll(path.Dir(p), 0o755)
	f.nodes[p] = &node{mode: fs.ModeSymlink | 0o777, target: target, modTime: f.modTime, inode: f.newInode()}
	return nil
}

// MkdirAll creates a directory and any missing parents.
func (f *FileSystem) MkdirAll(p string, perm fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuse("mkdir", p); err != nil {
		return err
	}
	f.mkdirAll(clean(p), perm)
	return nil
}

// CreateTemp creates a new empty file in dir, replacing "*" in pattern with a
// counter.
func (f *FileSystem) CreateTemp(dir, pattern string) (ports.TempFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuse("createtemp", dir); err != nil {
		return nil, err
	}
	f.tempSeq++
	name := strings.Replace(pattern, "*", fmt.Sprintf("%d", f.tempSeq), 1)
	if !strings.Contains(pattern, "*") {
		name = fmt.Sprintf("%s%d", pattern, f.tempSeq)
	}
	p := clean(path.Join(dir, name))
	f.mkdirAll(path.Dir(p), 0o755)
	f.nodes[p] = &node{mode: 0o600, modTime: f.modTime, inode: f.newInode()}
	return &tempFile{fsys: f, path: p}, nil
}

// OpenTemp reopens an existing partial file so a transfer can resume.
func (f *FileSystem) OpenTemp(p string) (ports.TempFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	if _, ok := f.nodes[p]; !ok {
		return nil, notExist("opentemp", p)
	}
	return &tempFile{fsys: f, path: p}, nil
}

// Rename moves a node, replacing any node already at newPath.
func (f *FileSystem) Rename(oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	oldPath, newPath = clean(oldPath), clean(newPath)
	if err := f.refuse("rename", newPath); err != nil {
		return err
	}
	if err, ok := f.renameFailures[newPath]; ok {
		delete(f.renameFailures, newPath)
		return err
	}
	n, ok := f.nodes[oldPath]
	if !ok {
		return notExist("rename", oldPath)
	}
	f.mkdirAll(path.Dir(newPath), 0o755)
	delete(f.nodes, oldPath)
	f.nodes[newPath] = n
	for p, child := range f.nodes {
		if strings.HasPrefix(p, childPrefix(oldPath)) {
			delete(f.nodes, p)
			f.nodes[newPath+strings.TrimPrefix(p, oldPath)] = child
		}
	}
	f.renames = append(f.renames, [2]string{oldPath, newPath})
	return nil
}

// SyncDir records that a directory was flushed. The in-memory filesystem has
// nothing to flush, so it only checks that the directory exists.
func (f *FileSystem) SyncDir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	n, ok := f.nodes[p]
	if !ok {
		return notExist("syncdir", p)
	}
	if !n.mode.IsDir() {
		return fmt.Errorf("syncdir %s: not a directory", p)
	}
	f.syncedDirs = append(f.syncedDirs, p)
	return nil
}

// SyncedDirs returns the directories passed to SyncDir, in order.
func (f *FileSystem) SyncedDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.syncedDirs...)
}

// Remove deletes a file or an empty directory.
func (f *FileSystem) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	if err := f.refuse("remove", p); err != nil {
		return err
	}
	n, ok := f.nodes[p]
	if !ok {
		return notExist("remove", p)
	}
	if n.mode.IsDir() {
		for other := range f.nodes {
			if strings.HasPrefix(other, childPrefix(p)) {
				return fmt.Errorf("remove %s: directory not empty", p)
			}
		}
	}
	delete(f.nodes, p)
	f.removes = append(f.removes, p)
	return nil
}

// Chtimes sets the modification time of p.
func (f *FileSystem) Chtimes(p string, mtime time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[clean(p)]
	if !ok {
		return notExist("chtimes", p)
	}
	n.modTime = mtime
	return nil
}

// Chmod sets the permission bits of p.
func (f *FileSystem) Chmod(p string, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[clean(p)]
	if !ok {
		return notExist("chmod", p)
	}
	n.mode = n.mode&^fs.ModePerm | mode.Perm()
	return nil
}

// infoLocked builds a FileInfo. The caller must hold f.mu.
func (f *FileSystem) infoLocked(p string, n *node) ports.FileInfo {
	return ports.FileInfo{
		Name:    path.Base(p),
		Size:    int64(len(n.data)),
		ModTime: n.modTime,
		Mode:    n.mode,
		Inode:   n.inode,
	}
}

// mkdirAll creates p and its parents. The caller must hold f.mu.
func (f *FileSystem) mkdirAll(p string, perm fs.FileMode) {
	if perm == 0 {
		perm = 0o755
	}
	for _, dir := range ancestors(p) {
		if _, ok := f.nodes[dir]; !ok {
			f.nodes[dir] = &node{mode: fs.ModeDir | perm.Perm(), modTime: f.modTime, inode: f.newInode()}
		}
	}
}

// tempFile is a handle on an in-memory file supporting random-access writes.
type tempFile struct {
	fsys *FileSystem
	path string
}

// WriteAt writes b at off, growing the file as needed.
func (t *tempFile) WriteAt(b []byte, off int64) (int, error) {
	t.fsys.mu.Lock()
	defer t.fsys.mu.Unlock()
	n, ok := t.fsys.nodes[t.path]
	if !ok {
		return 0, notExist("writeat", t.path)
	}
	if end := off + int64(len(b)); end > int64(len(n.data)) {
		grown := make([]byte, end)
		copy(grown, n.data)
		n.data = grown
	}
	copy(n.data[off:], b)
	return len(b), nil
}

// ReadAt reads len(b) bytes at off, returning io.EOF on a short read.
func (t *tempFile) ReadAt(b []byte, off int64) (int, error) {
	t.fsys.mu.Lock()
	defer t.fsys.mu.Unlock()
	n, ok := t.fsys.nodes[t.path]
	if !ok {
		return 0, notExist("readat", t.path)
	}
	if off >= int64(len(n.data)) {
		return 0, io.EOF
	}
	copied := copy(b, n.data[off:])
	if copied < len(b) {
		return copied, io.EOF
	}
	return copied, nil
}

// Truncate resizes the file to size bytes.
func (t *tempFile) Truncate(size int64) error {
	t.fsys.mu.Lock()
	defer t.fsys.mu.Unlock()
	n, ok := t.fsys.nodes[t.path]
	if !ok {
		return notExist("truncate", t.path)
	}
	resized := make([]byte, size)
	copy(resized, n.data)
	n.data = resized
	return nil
}

// Sync is a no-op for the in-memory filesystem.
func (t *tempFile) Sync() error { return nil }

// Close is a no-op for the in-memory filesystem.
func (t *tempFile) Close() error { return nil }

// Name returns the path of the temp file.
func (t *tempFile) Name() string { return t.path }

// clean normalizes p to an absolute slash path.
func clean(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// childPrefix returns the prefix every child of dir starts with.
func childPrefix(dir string) string {
	if dir == "/" {
		return "/"
	}
	return dir + "/"
}

// underAny reports whether p lies inside one of the given directories.
func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if p != d && strings.HasPrefix(p, childPrefix(d)) {
			return true
		}
	}
	return false
}

// relTo returns p relative to root, or "" when p is root.
func relTo(root, p string) string {
	if p == root {
		return ""
	}
	return strings.TrimPrefix(p, childPrefix(root))
}

// ancestors returns p and each of its parent directories, outermost first.
func ancestors(p string) []string {
	var out []string
	for cur := p; ; cur = path.Dir(cur) {
		out = append([]string{cur}, out...)
		if cur == "/" || cur == "." {
			break
		}
	}
	return out
}

// notExist builds an fs.ErrNotExist error for op on p.
func notExist(op, p string) error {
	return fmt.Errorf("%s %s: %w", op, p, fs.ErrNotExist)
}
