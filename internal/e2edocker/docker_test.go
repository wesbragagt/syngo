package e2edocker

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// docker runs the docker CLI. Every call is logged, so a failing test reads
// as the list of commands that were run.
type docker struct {
	t   *testing.T
	bin string
}

// result is what one docker command did.
type result struct {
	stdout string
	stderr string
	code   int
}

// run executes a docker command and returns its output. A command that could
// not be started at all, or that was killed by the context, is an error; a
// command that ran and exited non-zero is not, because a caller may be asking
// whether something is there.
func (d docker) run(ctx context.Context, args ...string) (result, error) {
	d.t.Helper()
	d.t.Logf("$ %s %s", d.bin, strings.Join(args, " "))

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	res := result{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		return res, err
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

// try runs a docker command under the default timeout.
func (d docker) try(args ...string) (result, error) {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	return d.run(ctx, args...)
}

// mustRun runs a docker command and fails the test unless it exits zero.
func (d docker) mustRun(args ...string) string {
	d.t.Helper()
	res, err := d.try(args...)
	if err != nil {
		d.t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	if res.code != 0 {
		d.t.Fatalf("docker %s: exit %d: %s%s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return strings.TrimSpace(res.stdout)
}

// quiet runs a docker command and reports nothing but whether it worked. It
// is for cleanup, where a container that is already gone is not a failure.
func (d docker) quiet(args ...string) {
	d.t.Helper()
	if _, err := d.try(args...); err != nil {
		d.t.Logf("docker %s: %v", strings.Join(args, " "), err)
	}
}

// exec runs a command inside a container. user is empty for the container's
// own user, or "uid:gid".
func (d docker) exec(ctr, user string, args ...string) result {
	d.t.Helper()
	res, err := d.try(d.execArgs(ctr, user, false, args)...)
	if err != nil {
		d.t.Fatalf("exec in %s: %v", ctr, err)
	}
	return res
}

// execDetached starts a command inside a container and returns at once.
func (d docker) execDetached(ctr, user string, args ...string) {
	d.t.Helper()
	d.mustRun(d.execArgs(ctr, user, true, args)...)
}

func (d docker) execArgs(ctr, user string, detach bool, args []string) []string {
	out := []string{"exec"}
	if detach {
		out = append(out, "-d")
	}
	if user != "" {
		out = append(out, "--user", user)
	}
	return append(append(out, ctr), args...)
}
