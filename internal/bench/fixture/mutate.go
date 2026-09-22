package fixture

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// Op names the kind of change a mutation made, for a runner to assert
// against.
type Op string

const (
	// OpRename moved a file from one relative path to another.
	OpRename Op = "rename"
	// OpDelete removed a file.
	OpDelete Op = "delete"
	// OpModify rewrote a file's content in place.
	OpModify Op = "modify"
	// OpEditorSave replaced a file's content the way Vim and Obsidian do:
	// write a sibling temporary file, then rename it over the target.
	OpEditorSave Op = "editor-save"
)

// MutationResult reports what a mutation did to a tree, so a runner can
// assert the expected change without re-deriving it.
type MutationResult struct {
	Op Op
	// Path is the file's path after the mutation, relative to root.
	Path string
	// OldPath is the file's path before the mutation. It is set for
	// OpRename only.
	OldPath string
	// Size is the resulting file's size in bytes. It is 0 after OpDelete.
	Size int64
}

// Rename moves the file at root/oldRel to root/newRel.
func Rename(root, oldRel, newRel string) (MutationResult, error) {
	oldFull := filepath.Join(root, filepath.FromSlash(oldRel))
	newFull := filepath.Join(root, filepath.FromSlash(newRel))
	if err := os.MkdirAll(filepath.Dir(newFull), 0o755); err != nil {
		return MutationResult{}, fmt.Errorf("fixture: mkdir for rename target %s: %w", newRel, err)
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		return MutationResult{}, fmt.Errorf("fixture: rename %s to %s: %w", oldRel, newRel, err)
	}
	info, err := os.Stat(newFull)
	if err != nil {
		return MutationResult{}, fmt.Errorf("fixture: stat renamed %s: %w", newRel, err)
	}
	return MutationResult{Op: OpRename, Path: newRel, OldPath: oldRel, Size: info.Size()}, nil
}

// Delete removes the file at root/rel.
func Delete(root, rel string) (MutationResult, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.Remove(full); err != nil {
		return MutationResult{}, fmt.Errorf("fixture: delete %s: %w", rel, err)
	}
	return MutationResult{Op: OpDelete, Path: rel}, nil
}

// Modify overwrites the file at root/rel in place with size bytes of
// deterministic text content, drawn from seed.
func Modify(root, rel string, seed int64, size int) (MutationResult, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)>>1|1))
	if err := os.WriteFile(full, randomBytes(rng, size, true), 0o644); err != nil {
		return MutationResult{}, fmt.Errorf("fixture: modify %s: %w", rel, err)
	}
	return MutationResult{Op: OpModify, Path: rel, Size: int64(size)}, nil
}

// EditorSave replaces the file at root/rel the way Vim and Obsidian save:
// it writes size bytes of deterministic text content to a temporary sibling
// file, then renames the temporary file over the target. On POSIX
// filesystems the rename is atomic, so a watcher never observes a
// zero-length or partially written target.
func EditorSave(root, rel string, seed int64, size int) (MutationResult, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	tmp := full + ".swp"
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)>>1|1))
	if err := os.WriteFile(tmp, randomBytes(rng, size, true), 0o644); err != nil {
		return MutationResult{}, fmt.Errorf("fixture: editor-save write temp for %s: %w", rel, err)
	}
	if err := os.Rename(tmp, full); err != nil {
		return MutationResult{}, fmt.Errorf("fixture: editor-save rename over %s: %w", rel, err)
	}
	return MutationResult{Op: OpEditorSave, Path: rel, Size: int64(size)}, nil
}
