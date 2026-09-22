package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files under a fresh directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

func TestHashTreeSkipsTheStateDirectory(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.txt":              "hello",
		"deep/b.txt":         "world",
		".syncthis/state.db": "bookkeeping",
	})
	got, err := hashTree(dir)
	if err != nil {
		t.Fatalf("hashTree: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("hashTree returned %d files, want 2 (the .syncthis directory is not content): %v", len(got), got)
	}
	// "hello" hashed with SHA-256, the same digest `sha256sum` prints on the
	// peer machine.
	const wantHello = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got["a.txt"] != wantHello {
		t.Errorf("hash of a.txt = %q, want %q", got["a.txt"], wantHello)
	}
}

// The peer's hashes arrive as sha256sum output. Reading them back has to
// produce exactly what hashTree produces here, or every comparison is a false
// difference.
func TestParseHashLinesMatchesHashTree(t *testing.T) {
	dir := writeTree(t, map[string]string{"a.txt": "hello", "deep/b.txt": "world"})
	local, err := hashTree(dir)
	if err != nil {
		t.Fatalf("hashTree: %v", err)
	}

	var b strings.Builder
	for path, sum := range local {
		b.WriteString(sum + "  ./" + path + "\n")
	}
	remote, err := parseHashLines(b.String())
	if err != nil {
		t.Fatalf("parseHashLines: %v", err)
	}
	if equal, diff := hashesEqual(local, remote); !equal {
		t.Errorf("a tree does not equal its own sha256sum output: %s", diff)
	}
}

func TestParseHashLinesRefusesUnreadableOutput(t *testing.T) {
	for _, out := range []string{"no-two-spaces-here", "  ./only-a-path"} {
		if _, err := parseHashLines(out); err == nil {
			t.Errorf("parseHashLines(%q) = nil, want an error", out)
		}
	}
}

// Two trees with the same number of files and the same names can still hold
// different bytes. Only the content hash can tell, which is why a run
// compares hashes before it records a convergence.
func TestHashesEqualCatchesTreesThatOnlyLookAlike(t *testing.T) {
	local := writeTree(t, map[string]string{"a.txt": "hello", "b.txt": "world"})
	remote := writeTree(t, map[string]string{"a.txt": "hello", "b.txt": "WORLD"})

	localHashes, err := hashTree(local)
	if err != nil {
		t.Fatalf("hashTree: %v", err)
	}
	remoteHashes, err := hashTree(remote)
	if err != nil {
		t.Fatalf("hashTree: %v", err)
	}
	if len(localHashes) != len(remoteHashes) {
		t.Fatalf("this test needs two trees with the same file count")
	}
	equal, diff := hashesEqual(localHashes, remoteHashes)
	if equal {
		t.Fatal("two trees with the same names but different content compared equal")
	}
	if !strings.Contains(diff, "b.txt") {
		t.Errorf("difference %q does not name the file that differs", diff)
	}
}

func TestHashesEqualReportsMissingAndExtraFiles(t *testing.T) {
	local := map[string]string{"a.txt": "1", "b.txt": "2"}
	remote := map[string]string{"a.txt": "1", "c.txt": "3"}
	equal, diff := hashesEqual(local, remote)
	if equal {
		t.Fatal("trees holding different paths compared equal")
	}
	for _, want := range []string{"b.txt", "c.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("difference %q does not mention %q", diff, want)
		}
	}
}
