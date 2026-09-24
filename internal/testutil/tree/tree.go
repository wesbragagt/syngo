// Package tree describes a folder on disk as a map of paths to content, so a
// test can say that two devices hold the same files. It is test-only helper
// code kept in a normal package because two separate test packages
// (internal/e2e and internal/e2edocker) share it; the architecture test makes
// sure only _test.go files import it.
package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wesbragagt/syngo/internal/adapters/blake3hash"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// Entry is what a comparison knows about one path of a folder.
type Entry struct {
	Path    string
	Kind    manifest.Kind
	Size    int64
	Mode    fs.FileMode
	Hash    string
	Target  string
	ModTime time.Time
}

// Read walks a folder root and describes everything in it, skipping the state
// directory.
func Read(root string) (map[string]Entry, error) {
	hasher := blake3hash.New()
	out := make(map[string]Entry)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		// A daemon may delete a path while the walk is reading it. A
		// tree that is still moving is compared again by the caller, so
		// what is already gone is left out rather than failing the test.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".syngo" {
			return filepath.SkipDir
		}

		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		entry := Entry{Path: rel, Mode: info.Mode().Perm(), ModTime: info.ModTime()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			entry.Kind = manifest.KindSymlink
			if entry.Target, err = os.Readlink(path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
		case info.IsDir():
			entry.Kind = manifest.KindDir
		default:
			entry.Kind = manifest.KindFile
			entry.Size = info.Size()
			f, err := os.Open(path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			hash, err := hasher.Sum(f)
			_ = f.Close()
			if err != nil {
				return err
			}
			entry.Hash = hash.String()
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read tree %s: %w", root, err)
	}
	return out, nil
}

// Diff describes how two folder trees differ, or is empty when they hold the
// same content.
func Diff(want, got map[string]Entry) []string {
	var problems []string
	for path, w := range want {
		g, ok := got[path]
		if !ok {
			problems = append(problems, "missing: "+path)
			continue
		}
		switch {
		case w.Kind != g.Kind:
			problems = append(problems, fmt.Sprintf("%s: kind %s, want %s", path, g.Kind, w.Kind))
		case w.Hash != g.Hash:
			problems = append(problems, fmt.Sprintf("%s: hash %s, want %s", path, g.Hash, w.Hash))
		case w.Target != g.Target:
			problems = append(problems, fmt.Sprintf("%s: link to %s, want %s", path, g.Target, w.Target))
		case w.Kind != manifest.KindSymlink && w.Mode != g.Mode:
			problems = append(problems, fmt.Sprintf("%s: mode %v, want %v", path, g.Mode, w.Mode))
		case w.Kind == manifest.KindFile && absDuration(w.ModTime.Sub(g.ModTime)) > time.Second:
			problems = append(problems, fmt.Sprintf("%s: mtime %s, want %s", path, g.ModTime, w.ModTime))
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			problems = append(problems, "unexpected: "+path)
		}
	}
	sort.Strings(problems)
	return problems
}

// absDuration returns the absolute value of d.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
