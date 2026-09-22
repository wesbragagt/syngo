// Package tailscalecli asks the local Tailscale client what it knows, by
// running `tailscale status --json`. It is the only place in the program that
// shells out, and nothing but the `network` commands and the host name
// convenience of `peer add` depends on it.
package tailscalecli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// waitDelay bounds how long Run waits for the output pipes after the command
// was cancelled. A process that forks a child which inherits the pipe keeps it
// open after the parent is killed, and without this Wait would block for as
// long as that grandchild lives, whatever the context said.
const waitDelay = time.Second

// Runner looks commands up and runs them. It exists so the adapter can be
// tested without a Tailscale installation.
type Runner interface {
	// LookPath returns the path of an executable, or an error when it is
	// not installed.
	LookPath(file string) (string, error)
	// Run executes path with args and returns its standard output. A
	// failure carries the standard error.
	Run(ctx context.Context, path string, args ...string) ([]byte, error)
}

// ExecRunner runs commands as child processes. The caller decides the timeout
// through the context it passes.
type ExecRunner struct{}

// LookPath searches PATH for the executable.
func (ExecRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Run executes the command and returns its standard output. The standard
// error is folded into the error, trimmed to one line, because that is what a
// user needs to see when Tailscale refuses.
//
// The command runs in a process group of its own so that cancelling it kills
// whatever it forked as well: the deadline the caller set is a promise about
// when Run returns, and a wrapper script that leaves a child behind must not
// be able to break it.
func (ExecRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	isolateProcessGroup(cmd)
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%s did not answer: %w", path, ctxErr)
		}
		if message := firstLine(stderr.String()); message != "" {
			return nil, fmt.Errorf("%v: %s", err, message)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
