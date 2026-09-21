package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Reserved names inside a folder. They belong to this device alone and are
// never part of a manifest, whatever a peer claims.
const (
	// ReservedDir is the per-folder state directory, holding the partial
	// downloads among other things.
	ReservedDir = ".syncthis"
	// IgnoreFile is the per-folder ignore file. It is the local user's
	// statement about what this device wants; a peer may not write it.
	IgnoreFile = ".syncthisignore"
)

// ErrInvalidPath reports a manifest path that may not be used: one that is
// empty, absolute, contains a ".." or an empty segment, carries a NUL byte, or
// names one of the reserved paths of a folder.
var ErrInvalidPath = errors.New("invalid manifest path")

// ErrInvalidLinkTarget reports a symlink target that points outside its
// folder, either because it is absolute or because it climbs out with "..".
var ErrInvalidLinkTarget = errors.New("invalid symlink target")

// ValidRelPath reports whether p may be used as a folder-relative manifest
// path. It is the single place that decides which paths a peer may name, so a
// hostile manifest cannot reach outside the folder root.
//
// A valid path is slash-separated, relative, free of "." and ".." segments and
// of empty segments, carries no NUL byte, and is neither the reserved state
// directory nor the ignore file, nor anything beneath them.
func ValidRelPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty", ErrInvalidPath)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%w: %q contains a NUL byte", ErrInvalidPath, p)
	case !fs.ValidPath(p):
		// fs.ValidPath rejects a leading slash, a trailing slash, an
		// empty segment and every "." or ".." segment.
		return fmt.Errorf("%w: %q", ErrInvalidPath, p)
	case p == ".":
		// fs.ValidPath accepts "." as the root of a tree; a manifest
		// entry for the folder root itself is meaningless.
		return fmt.Errorf("%w: %q names the folder root", ErrInvalidPath, p)
	case reserved(p):
		return fmt.Errorf("%w: %q is reserved", ErrInvalidPath, p)
	}
	return nil
}

// reserved reports whether p is one of the reserved paths of a folder, or
// lies beneath one.
func reserved(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return first == ReservedDir || first == IgnoreFile
}

// ValidLinkTarget reports whether target may be stored as the target of a
// symlink at linkPath, which must itself be a valid relative path. Targets are
// interpreted relative to the directory holding the link and must stay inside
// the folder: an absolute target, or one that climbs out of the root, would
// let a peer write through the link into the rest of the filesystem.
func ValidLinkTarget(linkPath, target string) error {
	switch {
	case target == "":
		return fmt.Errorf("%w: empty", ErrInvalidLinkTarget)
	case strings.ContainsRune(target, 0):
		return fmt.Errorf("%w: %q contains a NUL byte", ErrInvalidLinkTarget, target)
	case strings.HasPrefix(target, "/"):
		return fmt.Errorf("%w: %q is absolute", ErrInvalidLinkTarget, target)
	}
	if err := ValidRelPath(linkPath); err != nil {
		return err
	}

	resolved := path.Join(path.Dir(linkPath), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%w: %q from %q leaves the folder", ErrInvalidLinkTarget, target, linkPath)
	}
	if resolved != "." && reserved(resolved) {
		return fmt.Errorf("%w: %q from %q names a reserved path", ErrInvalidLinkTarget, target, linkPath)
	}
	return nil
}
