package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// commandRunner runs one external program and returns what it wrote to
// stdout. It exists so the ssh and scp command lines this package builds can
// be asserted in a test without a network: the tests substitute a recorder,
// the real runs use execRunner.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner runs a program with os/exec.
type execRunner struct{}

// Run implements commandRunner. A failure carries the program's stderr,
// because an ssh failure says what went wrong there and nowhere else.
func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// sshOptions are the options every ssh and scp invocation carries.
// BatchMode refuses to prompt: a run must fail with "permission denied"
// rather than block on a passphrase prompt nobody is watching.
var sshOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
}

// sshArgs is the argument list for running script on host through ssh.
func sshArgs(host, script string) []string {
	args := append([]string{}, sshOptions...)
	return append(args, host, script)
}

// scpArgs is the argument list for copying localPath to remotePath on host.
func scpArgs(localPath, host, remotePath string) []string {
	args := append([]string{}, sshOptions...)
	return append(args, localPath, host+":"+remotePath)
}

// shellQuote wraps s so a POSIX shell on the peer machine treats it as one
// literal word, whatever it holds.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin quotes every word and joins them into one command line.
func shellJoin(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}
