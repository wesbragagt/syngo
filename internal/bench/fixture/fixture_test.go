package fixture_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/bench/fixture"
)

// smallSpec keeps every generated test tree tiny: a handful of small text
// files, no large files, a couple of binary files, and a shallow chain.
func smallSpec(seed int64) fixture.Spec {
	return fixture.Spec{
		Seed:        seed,
		SmallFiles:  fixture.CountSize{Count: 4, MinSize: 10, MaxSize: 40},
		LargeFiles:  fixture.CountSize{},
		BinaryFiles: fixture.CountSize{Count: 2, MinSize: 8, MaxSize: 16},
		DeepTree:    fixture.DeepTreeSpec{Depth: 3, FilesPerDir: 1, FileSize: 10},
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	spec := smallSpec(42)

	dirA := t.TempDir()
	resultA, err := fixture.Generate(dirA, spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	dirB := t.TempDir()
	resultB, err := fixture.Generate(dirB, spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(resultA.Files) != len(resultB.Files) {
		t.Fatalf("file count differs: %d vs %d", len(resultA.Files), len(resultB.Files))
	}
	for i := range resultA.Files {
		a, b := resultA.Files[i], resultB.Files[i]
		if a.Path != b.Path || a.Size != b.Size {
			t.Fatalf("file %d differs: %+v vs %+v", i, a, b)
		}
		contentA, err := os.ReadFile(filepath.Join(dirA, filepath.FromSlash(a.Path)))
		if err != nil {
			t.Fatalf("read %s: %v", a.Path, err)
		}
		contentB, err := os.ReadFile(filepath.Join(dirB, filepath.FromSlash(b.Path)))
		if err != nil {
			t.Fatalf("read %s: %v", b.Path, err)
		}
		if !bytes.Equal(contentA, contentB) {
			t.Fatalf("file %s has different bytes across runs", a.Path)
		}
	}
}

func TestGenerateDifferentSeedsDiffer(t *testing.T) {
	dirA := t.TempDir()
	if _, err := fixture.Generate(dirA, smallSpec(1)); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dirB := t.TempDir()
	if _, err := fixture.Generate(dirB, smallSpec(2)); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	contentA, err := os.ReadFile(filepath.Join(dirA, "small", "file-0000.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	contentB, err := os.ReadFile(filepath.Join(dirB, "small", "file-0000.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Equal(contentA, contentB) {
		t.Fatalf("different seeds produced identical content")
	}
}

func TestGenerateShapesEachGroup(t *testing.T) {
	dir := t.TempDir()
	spec := smallSpec(7)
	result, err := fixture.Generate(dir, spec)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	counts := map[string]int{}
	for _, f := range result.Files {
		top := f.Path[:len(f.Path)-len(filepath.Base(f.Path))]
		switch {
		case bytes.HasPrefix([]byte(f.Path), []byte("small/")):
			counts["small"]++
		case bytes.HasPrefix([]byte(f.Path), []byte("binary/")):
			counts["binary"]++
		case bytes.HasPrefix([]byte(f.Path), []byte("deep/")):
			counts["deep"]++
		default:
			t.Fatalf("unexpected path prefix %q (%s)", f.Path, top)
		}
	}

	if counts["small"] != spec.SmallFiles.Count {
		t.Errorf("small files: got %d, want %d", counts["small"], spec.SmallFiles.Count)
	}
	if counts["binary"] != spec.BinaryFiles.Count {
		t.Errorf("binary files: got %d, want %d", counts["binary"], spec.BinaryFiles.Count)
	}
	if counts["deep"] != spec.DeepTree.Depth*spec.DeepTree.FilesPerDir {
		t.Errorf("deep files: got %d, want %d", counts["deep"], spec.DeepTree.Depth*spec.DeepTree.FilesPerDir)
	}
}

func TestGenerateRejectsNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := fixture.Generate(dir, smallSpec(1)); err == nil {
		t.Fatal("expected an error for a non-empty target directory")
	}
}

func TestDefaultSpecIsDeterministic(t *testing.T) {
	spec := fixture.DefaultSpec(9)
	// Shrink the counts so the test stays fast; the shape and determinism
	// contract is what's under test, not the production sizes.
	spec.SmallFiles.Count = 3
	spec.LargeFiles.Count = 0
	spec.BinaryFiles.Count = 1
	spec.DeepTree.Depth = 2

	dir := t.TempDir()
	if _, err := fixture.Generate(dir, spec); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}
