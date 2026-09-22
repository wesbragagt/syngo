//go:build windows

package tailscalecli

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup gives the command its own process group. Windows has no
// group kill through a negative pid, so cancellation stays the default, which
// kills the command itself; a grandchild it left behind is bounded by
// cmd.WaitDelay instead.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
