package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// syncthisBinary is the binary the crash tests kill. It is empty when the
// build was skipped or failed; buildErr says which.
var (
	syncthisBinary string
	buildErr       error
	// buildDir holds the binary and the fixture tree the crash tests share.
	buildDir string
)

// TestMain builds the syncthis binary once for the whole package, because the
// crash tests need a real process to send SIGKILL to and building it per test
// would cost more than the tests themselves. A missing Go toolchain is not a
// failure here: the tests that need the binary skip.
func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "syncthis-e2e-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: temp dir: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		buildDir = dir
		syncthisBinary, buildErr = buildBinary(dir)
		return m.Run()
	}()
	os.Exit(code)
}

// buildBinary compiles cmd/syncthis into dir. It is built without -race even
// when the test binary has it: the child is what gets killed, so its build
// mode says nothing about the claim under test, and a race build would make
// every crash test several times slower.
func buildBinary(dir string) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("no go toolchain on PATH: %w", err)
	}
	binary := filepath.Join(dir, "syncthis")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/syncthis")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v: %s", err, out)
	}
	return binary, nil
}
