package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The scale tests measure what the task list calls the scale and resource
// claims: a 5,000-file folder, idle memory, a 10 GiB transfer, a thousand
// interruption cycles and a bandwidth limit held to within 5%. Each one runs
// for minutes and needs disk the rest of the suite does not, so they are
// opt-in exactly as internal/e2edocker is: without SYNGO_SCALE=1 they skip
// before they allocate anything, and `make test` only pays for compiling
// them. `make scale` runs them.
const (
	// scaleEnv gates every test in this group.
	scaleEnv = "SYNGO_SCALE"
	// scaleDirEnv names a directory the scale tests put their data,
	// folders and fixtures in. The default is the system temporary
	// directory, which on a machine where /tmp is a tmpfs is far too small
	// for the 10 GiB transfer: `make scale` passes a directory on disk.
	scaleDirEnv = "SYNGO_SCALE_DIR"
)

// systemTemp is the system temporary directory as it was before any scale
// test pointed TMPDIR at its own scratch directory. The control socket of a
// daemon lives in its data directory, and a unix socket path is limited to
// about 100 characters by the kernel, which a scratch directory deep inside a
// checkout blows through on its own. The pair harness therefore keeps its
// data directories here, short, and only the folder roots and the staged
// files, which are the large ones, in the scratch directory.
var systemTemp = os.TempDir()

// scaleShortDir returns a directory with a short path, for data directories
// whose control socket has to fit in a sockaddr.
func scaleShortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(systemTemp, "st")
	if err != nil {
		t.Fatalf("short directory in %s: %v", systemTemp, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("remove %s: %v", dir, err)
		}
	})
	return dir
}

// requireScale skips a test unless the scale group was asked for.
func requireScale(t *testing.T) {
	t.Helper()
	if os.Getenv(scaleEnv) != "1" {
		t.Skipf("scale test: set %s=1 (see make scale)", scaleEnv)
	}
}

// scaleEnvInt reads a whole number from the environment, or returns fallback
// when it is unset, unreadable or below one. Every size and count of this
// group is a parameter, so a machine with less disk or less time than the
// full claim needs can still run a smaller version of the same test and
// report the number it really ran.
func scaleEnvInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("%s = %q, want a whole number of at least 1", key, raw)
	}
	return n
}

// scaleScratch returns an empty directory for one test to work in, and points
// TMPDIR at it so everything the harness creates with t.TempDir lands there
// too. It is removed when the test ends.
func scaleScratch(t *testing.T) string {
	t.Helper()
	base := os.Getenv(scaleDirEnv)
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create %s %s: %v", scaleDirEnv, base, err)
	}
	dir, err := os.MkdirTemp(base, "syngo-scale-")
	if err != nil {
		t.Fatalf("scratch directory in %s: %v", base, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("remove scratch %s: %v", dir, err)
		}
	})
	t.Setenv("TMPDIR", dir)
	free, err := freeBytes(dir)
	if err == nil {
		t.Logf("scratch %s, %s free", dir, humanBytes(free))
	}
	return dir
}

// freeBytes is how many bytes an unprivileged process may still write into
// the filesystem holding dir. The 10 GiB transfer checks it before it starts:
// a run that would fill the disk is refused with the real numbers rather than
// failing halfway through with a write error.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}

// procStatusField reads one VmRSS-style field of /proc/<pid>/status, in bytes.
// It is Linux only on purpose: a test that cannot read the real number skips
// with that as the reason rather than passing on an assumption.
func procStatusField(pid int, field string) (int64, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "status")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || name != field {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("%s: %s is %q, want a value in kB", path, field, strings.TrimSpace(value))
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s: %w", path, field, err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("%s holds no %s", path, field)
}

// residentBytes is the resident set size of a process right now.
func residentBytes(pid int) (int64, error) { return procStatusField(pid, "VmRSS") }

// peakResidentBytes is the highest resident set size a process ever reached,
// as the kernel recorded it. It covers the whole life of the process,
// including what it did before a sampler was started.
func peakResidentBytes(pid int) (int64, error) { return procStatusField(pid, "VmHWM") }

// requireProcMemory skips the test when this machine cannot report a
// process's resident memory. Every memory claim in this group is measured
// from /proc, and there is no portable stdlib source for another process's
// RSS; adding a dependency to get one is out of scope.
func requireProcMemory(t *testing.T) {
	t.Helper()
	if _, err := residentBytes(os.Getpid()); err != nil {
		t.Skipf("this platform does not report resident memory: %v", err)
	}
}

// rssSampler polls the resident memory of a process and keeps the highest
// reading. It is how a transfer's memory is bounded while it runs rather than
// after it, and it complements VmHWM: the kernel's peak is authoritative, the
// samples say when the peak happened.
type rssSampler struct {
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	peak    int64
	samples int
	err     error
}

// sampleRSS starts polling a process every interval until stop is called.
func sampleRSS(pid int, interval time.Duration) *rssSampler {
	s := &rssSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			s.sample(pid)
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}

// sample takes one reading. A process that has exited is not an error: the
// sampler stops finding it and keeps what it saw.
func (s *rssSampler) sample(pid int) {
	rss, err := residentBytes(pid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.samples == 0 {
			s.err = err
		}
		return
	}
	s.samples++
	if rss > s.peak {
		s.peak = rss
	}
}

// Stop ends the sampling and returns the highest reading and how many
// readings it is based on.
func (s *rssSampler) Stop() (peak int64, samples int, err error) {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak, s.samples, s.err
}

// humanBytes formats a byte count for a log line.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
