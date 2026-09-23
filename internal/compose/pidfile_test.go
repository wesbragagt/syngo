package compose

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// deadPID returns the pid of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run helper process: %v", err)
	}
	return cmd.Process.Pid
}

func TestAcquirePIDFile_WritesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")

	release, err := acquirePIDFile(path, os.Getpid())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if got := string(data); got != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("pid file = %q, want our pid", got)
	}

	release()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file still exists after release (err %v)", err)
	}
}

func TestAcquirePIDFile_RefusesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")

	release, err := acquirePIDFile(path, os.Getpid())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	if _, err := acquirePIDFile(path, os.Getpid()); !errors.Is(err, ErrDaemonRunning) {
		t.Fatalf("second acquire = %v, want ErrDaemonRunning", err)
	}
}

// TestAcquirePIDFile_ReclaimsAReusedPID is the regression test for a daemon
// killed with SIGKILL, whose pid file survives it and whose pid is then handed
// to an unrelated process. Inside a container pids are small and recycled
// fast, so this is what a restart finds, and the daemon used to refuse to
// start for good. The parent of the test runner stands in for the unrelated
// process: it is alive, it is not a daemon, and it holds no lock.
func TestAcquirePIDFile_ReclaimsAReusedPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	reused := os.Getppid()
	if !processAlive(reused) {
		t.Fatalf("the parent of the test runner (pid %d) is not alive", reused)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(reused)+"\n"), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	release, err := acquirePIDFile(path, os.Getpid())
	if err != nil {
		t.Fatalf("acquire over a pid file naming a reused pid: %v", err)
	}
	defer release()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if got := string(data); got != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("pid file = %q, want our pid", got)
	}
}

func TestAcquirePIDFile_ReplacesAStaleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(deadPID(t))+"\n"), 0o600); err != nil {
		t.Fatalf("write stale pid file: %v", err)
	}

	release, err := acquirePIDFile(path, os.Getpid())
	if err != nil {
		t.Fatalf("acquire over a stale pid file: %v", err)
	}
	release()
}

func TestAcquirePIDFile_IgnoresGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	if err := os.WriteFile(path, []byte("not a pid"), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	release, err := acquirePIDFile(path, os.Getpid())
	if err != nil {
		t.Fatalf("acquire over a garbage pid file: %v", err)
	}
	release()
}

// processAlive reports whether a process with that pid exists. It is only a
// test helper now: the daemon no longer decides anything from pid liveness.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}
