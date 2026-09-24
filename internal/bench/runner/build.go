package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// buildStaticBinary compiles cmd/syngo for goos/goarch into dest with
// CGO_ENABLED=0.
//
// The static build is not an optimization. A dynamically linked build made on
// one NixOS machine names its interpreter by an absolute /nix/store path that
// does not exist on the other machine, so the copied binary dies with exit
// 127 before it prints anything. `make release` builds with the same flags.
func buildStaticBinary(ctx context.Context, dest, goos, goarch string) error {
	dir, err := moduleDir(ctx)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", dest, "./cmd/syngo")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build a static syngo for %s/%s: %w: %s",
			goos, goarch, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// moduleDir returns the directory holding this module's go.mod, so a build
// works from wherever the operator invoked the runner.
func moduleDir(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("locate the syngo module: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("no go.mod in scope: run the benchmark from inside the syngo checkout")
	}
	return strings.TrimSuffix(gomod, "/go.mod"), nil
}

// goPlatform maps what `uname -s -m` printed on the peer machine onto a
// GOOS/GOARCH pair, so the binary copied there can actually run.
func goPlatform(uname string) (goos, goarch string, err error) {
	fields := strings.Fields(uname)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unreadable `uname -s -m` output %q, want two words", strings.TrimSpace(uname))
	}
	switch strings.ToLower(fields[0]) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("the peer machine reports an unsupported operating system %q", fields[0])
	}
	switch fields[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("the peer machine reports an unsupported architecture %q", fields[1])
	}
	return goos, goarch, nil
}
