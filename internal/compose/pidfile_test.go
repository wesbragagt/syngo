package compose

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestAcquirePIDFile_RefusesALiveOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	// The parent process of the test runner is alive and is not us.
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	if _, err := acquirePIDFile(path, os.Getpid()); !errors.Is(err, ErrDaemonRunning) {
		t.Fatalf("acquire = %v, want ErrDaemonRunning", err)
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

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatalf("processAlive(self) = false")
	}
	if processAlive(deadPID(t)) {
		t.Fatalf("processAlive(dead) = true")
	}
}
