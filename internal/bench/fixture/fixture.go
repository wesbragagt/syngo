// Package fixture builds and mutates deterministic file trees for the
// benchmark corpus described in .specs/tasks.md, Phase 0.
//
// Generate writes a fresh tree from a Spec. Rename, Delete, Modify and
// EditorSave change an existing tree in place and report what they did, so a
// runner can assert the expected result. Every operation is a pure function
// of its seed: the same seed and the same spec produce the same bytes on any
// machine.
package fixture

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// CountSize describes a group of generated files: how many, and the
// inclusive byte range each one is drawn from.
type CountSize struct {
	// Count is how many files to create.
	Count int
	// MinSize and MaxSize bound each file's size in bytes, inclusive.
	MinSize, MaxSize int
}

// DeepTreeSpec describes a chain of nested directories used to exercise deep
// paths.
type DeepTreeSpec struct {
	// Depth is how many directories deep the chain goes.
	Depth int
	// FilesPerDir is how many small text files to place in each directory
	// along the chain.
	FilesPerDir int
	// FileSize is the size in bytes of each file in the chain.
	FileSize int
}

// Spec configures Generate. Every group is independent: set a Count to 0 to
// skip it.
type Spec struct {
	// Seed makes the tree reproducible. The same Seed and Spec always
	// produce the same files, names and bytes.
	Seed int64
	// SmallFiles is a group of small text files, written under "small/".
	SmallFiles CountSize
	// LargeFiles is a group of large files, written under "large/".
	LargeFiles CountSize
	// BinaryFiles is a group of files with uniformly random bytes, written
	// under "binary/".
	BinaryFiles CountSize
	// DeepTree is a chain of nested directories, written under "deep/".
	DeepTree DeepTreeSpec
}

// DefaultSpec returns the default corpus shape: 200 small text files
// (100 B to 2 KiB), 3 large files (5 MiB to 20 MiB), 5 binary files (1 KiB to
// 8 KiB), and a directory chain 10 levels deep with 2 files per level.
func DefaultSpec(seed int64) Spec {
	return Spec{
		Seed:        seed,
		SmallFiles:  CountSize{Count: 200, MinSize: 100, MaxSize: 2000},
		LargeFiles:  CountSize{Count: 3, MinSize: 5 << 20, MaxSize: 20 << 20},
		BinaryFiles: CountSize{Count: 5, MinSize: 1 << 10, MaxSize: 8 << 10},
		DeepTree:    DeepTreeSpec{Depth: 10, FilesPerDir: 2, FileSize: 500},
	}
}

// FileResult describes one file Generate wrote.
type FileResult struct {
	// Path is the file's path relative to the target directory, using
	// forward slashes.
	Path string
	// Size is the file's size in bytes.
	Size int64
}

// Result is everything Generate wrote, in the order it was written.
type Result struct {
	Files []FileResult
}

// Generate writes spec's corpus under dir, creating dir if needed. dir must
// either not exist or be empty; Generate never overwrites an existing tree.
func Generate(dir string, spec Spec) (Result, error) {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) > 0 {
		return Result{}, fmt.Errorf("fixture: %s is not empty", dir)
	}
	if err != nil && !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("fixture: read %s: %w", dir, err)
	}

	rng := rand.New(rand.NewPCG(uint64(spec.Seed), uint64(spec.Seed)>>1|1))
	var result Result

	groups := []struct {
		prefix string
		text   bool
		group  CountSize
	}{
		{"small", true, spec.SmallFiles},
		{"large", false, spec.LargeFiles},
		{"binary", false, spec.BinaryFiles},
	}
	for _, g := range groups {
		for i := 0; i < g.group.Count; i++ {
			size := sizeIn(rng, g.group.MinSize, g.group.MaxSize)
			rel := filepath.ToSlash(filepath.Join(g.prefix, fmt.Sprintf("file-%04d%s", i, extFor(g.text))))
			if err := writeFile(dir, rel, rng, size, g.text); err != nil {
				return Result{}, err
			}
			result.Files = append(result.Files, FileResult{Path: rel, Size: int64(size)})
		}
	}

	if spec.DeepTree.Depth > 0 {
		files, err := generateDeepTree(dir, rng, spec.DeepTree)
		if err != nil {
			return Result{}, err
		}
		result.Files = append(result.Files, files...)
	}

	return result, nil
}

// generateDeepTree writes a chain of nested directories "deep/1/2/3/..." up
// to spec.Depth levels, placing spec.FilesPerDir text files in each.
func generateDeepTree(dir string, rng *rand.Rand, spec DeepTreeSpec) ([]FileResult, error) {
	var files []FileResult
	segments := []string{"deep"}
	for level := 1; level <= spec.Depth; level++ {
		segments = append(segments, fmt.Sprintf("%d", level))
		dirRel := filepath.ToSlash(filepath.Join(segments...))
		for i := 0; i < spec.FilesPerDir; i++ {
			rel := filepath.ToSlash(filepath.Join(dirRel, fmt.Sprintf("file-%02d.txt", i)))
			if err := writeFile(dir, rel, rng, spec.FileSize, true); err != nil {
				return nil, err
			}
			files = append(files, FileResult{Path: rel, Size: int64(spec.FileSize)})
		}
	}
	return files, nil
}

// sizeIn draws a size uniformly from [min, max]. When min equals max it
// returns that size without consuming randomness.
func sizeIn(rng *rand.Rand, min, max int) int {
	if max <= min {
		return min
	}
	return min + rng.IntN(max-min+1)
}

// extFor returns the file extension for a text or binary file.
func extFor(text bool) string {
	if text {
		return ".txt"
	}
	return ".bin"
}

// writeFile writes size bytes of deterministic content, drawn from rng, to
// dir/rel, creating parent directories as needed. Text content is restricted
// to printable ASCII and newlines; binary content is unrestricted bytes.
func writeFile(dir, rel string, rng *rand.Rand, size int, text bool) error {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("fixture: mkdir for %s: %w", rel, err)
	}
	data := randomBytes(rng, size, text)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return fmt.Errorf("fixture: write %s: %w", rel, err)
	}
	return nil
}

// randomBytes returns size pseudo-random bytes drawn from rng. Text content
// is printable ASCII (0x20-0x7e) with an occasional newline; binary content
// covers the full byte range.
func randomBytes(rng *rand.Rand, size int, text bool) []byte {
	data := make([]byte, size)
	for i := range data {
		if !text {
			data[i] = byte(rng.IntN(256))
			continue
		}
		if i > 0 && i%72 == 0 {
			data[i] = '\n'
			continue
		}
		data[i] = byte(0x20 + rng.IntN(0x7f-0x20))
	}
	return data
}
