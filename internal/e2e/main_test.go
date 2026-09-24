package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// syngoBinary is the binary the crash tests kill. It is empty when the
// build was skipped or failed; buildErr says which. Both are only settled once
// binary() has run.
var (
	syngoBinary string
	buildErr    error
	buildOnce   sync.Once
	// buildDir holds the binary and the fixture tree the crash tests share.
	buildDir string
)

// binary builds cmd/syngo on the first call and returns it. It is built
// lazily so a run that needs no child process, such as one under -short, pays
// nothing for it.
func binary() (string, error) {
	buildOnce.Do(func() { syngoBinary, buildErr = buildBinary(buildDir) })
	return syngoBinary, buildErr
}

// TestMain makes the directory the binary and the fixture tree of the crash
// tests share. The binary itself is built by the first test that asks for it,
// once for the whole package: those tests need a real process to send SIGKILL
// to, and building it per test would cost more than the tests themselves. A
// missing Go toolchain is not a failure: the tests that need the binary skip.
func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "syngo-e2e-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: temp dir: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		buildDir = dir
		return m.Run()
	}()
	os.Exit(code)
}

// buildBinary compiles cmd/syngo into dir. It is built without -race even
// when the test binary has it: the child is what gets killed, so its build
// mode says nothing about the claim under test, and a race build would make
// every crash test several times slower.
func buildBinary(dir string) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("no go toolchain on PATH: %w", err)
	}
	binary := filepath.Join(dir, "syngo")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/syngo")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v: %s", err, out)
	}
	return binary, nil
}

// TestBuild_OnlyHappensWhenABinaryIsNeeded checks that a run which needs no
// child process does not pay for compiling one. It only claims anything under
// -short, where every test that wants the binary skips before asking for it.
func TestBuild_OnlyHappensWhenABinaryIsNeeded(t *testing.T) {
	if !testing.Short() {
		t.Skip("the other tests build the binary; run with -short")
	}
	if syngoBinary != "" || buildErr != nil {
		t.Fatalf("the binary was built (%q, %v) although no test asked for one",
			syngoBinary, buildErr)
	}
}
