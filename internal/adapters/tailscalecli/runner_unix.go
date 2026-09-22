//go:build unix

package tailscalecli

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the command in a process group of its own and makes
// cancellation kill the whole group, so a wrapper script that forks a child
// cannot outlive the deadline the caller set.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
