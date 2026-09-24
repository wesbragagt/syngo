package tailscalecli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/adapters/tailscalecli"
)

// TestExecRunner_CancelsAForkedChild checks that the deadline holds when the
// command forks: the script below starts `sleep` without exec, so the shell
// that gets killed leaves a grandchild holding the standard output pipe. Wait
// must not block on it.
func TestExecRunner_CancelsAForkedChild(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH to fork a child with")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep on PATH to fork a child with")
	}

	script := filepath.Join(t.TempDir(), "forks")
	// The sleep runs in the background and the shell waits for it, so the
	// grandchild inherits the pipe and outlives its parent.
	body := "#!" + shell + "\n" + sleep + " 60 &\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := tailscalecli.ExecRunner{}.Run(ctx, script)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run returned no error, want the cancellation")
		}
		if elapsed := time.Since(start); elapsed > 6*time.Second {
			t.Fatalf("Run took %s, want it bounded by the context", elapsed)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("Run did not return within 6s: the deadline does not hold across a fork")
	}
}

// TestExecRunner_ReportsStandardError folds the first line of standard error
// into the failure, which is what a user needs to see when Tailscale refuses.
func TestExecRunner_ReportsStandardError(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}

	script := filepath.Join(t.TempDir(), "refuses")
	body := "#!" + shell + "\necho 'backend not running' >&2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the script: %v", err)
	}

	_, err = tailscalecli.ExecRunner{}.Run(context.Background(), script)
	if err == nil {
		t.Fatalf("Run returned no error, want the exit status")
	}
	if got := err.Error(); !strings.Contains(got, "backend not running") {
		t.Fatalf("error = %q, want it to carry the standard error", got)
	}
}
