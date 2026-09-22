package e2edocker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// smallFileCount is how many files the scale test spreads over
// smallFileDirs directories.
const (
	smallFileCount = 10000
	smallFileDirs  = 100
)

// bigFileMiB is how large the single-file scale test makes its file. A run
// with -short moves far less; SYNCTHIS_E2E_BIG_MIB overrides both.
func bigFileMiB(t *testing.T) int {
	t.Helper()
	if raw := os.Getenv("SYNCTHIS_E2E_BIG_MIB"); raw != "" {
		mib, err := strconv.Atoi(raw)
		if err != nil || mib <= 0 {
			t.Fatalf("SYNCTHIS_E2E_BIG_MIB=%q is not a positive number of MiB", raw)
		}
		return mib
	}
	if testing.Short() {
		return 64
	}
	return 512
}

// TestScale_TenThousandSmallFiles is the case where the count is the load
// rather than the size: ten thousand files in a hundred directories, none of
// them large enough to matter on its own. Every device has to end up with all
// of them, and say so.
func TestScale_TenThousandSmallFiles(t *testing.T) {
	c := Start(t, 3)
	a := c.Device(0)

	files := smallFileCount
	if testing.Short() {
		files = 1000
	}
	start := time.Now()
	for i := 0; i < files; i++ {
		size := 100 + i%1901
		a.WriteFile(fmt.Sprintf("d%02d/f%05d.bin", i%smallFileDirs, i), payload(size, byte(i)))
	}
	t.Logf("wrote %d files on %s in %s", files, a.Name, time.Since(start).Round(time.Millisecond))

	// Ten thousand files take more than one folder's worth of time to
	// cross, so the wait is given the room the plan allows it.
	converge := time.Now()
	c.WaitConvergedWithin(10 * convergeTimeout)
	elapsed := time.Since(converge)
	t.Logf("%d files reached two devices in %s (%.0f files/s per device)",
		files, elapsed.Round(time.Millisecond), float64(files)/elapsed.Seconds())

	// The standard invariant check is spelled out here because this
	// scenario also asserts that a burst of this size produces no error
	// line at all: a scan overtaken by the puller is rescheduled, not
	// reported.
	for _, dev := range c.Devices {
		if parts := dev.PartFiles(); len(parts) != 0 {
			t.Errorf("%s left partial downloads: %v", dev.Name, parts)
		}
		if rows := dev.Transfers(); len(rows) != 0 {
			t.Errorf("%s has %d transfers left in its journal", dev.Name, len(rows))
		}
		for _, line := range dev.LogLines("error") {
			if expectedError(line.Msg) {
				continue
			}
			t.Errorf("%s logged an error: %s", dev.Name, line.Raw)
		}
	}

	for _, dev := range c.Devices {
		folders := dev.Status().Folders
		if len(folders) != 1 {
			t.Fatalf("%s reports %d folders, want one", dev.Name, len(folders))
		}
		if got := folders[0].Files; got != int64(files) {
			t.Errorf("%s counts %d files, want %d", dev.Name, got, files)
		}
		if got := folders[0].Dirs; got != int64(smallFileDirs) {
			t.Errorf("%s counts %d directories, want %d", dev.Name, got, smallFileDirs)
		}
	}
}

// TestScale_LargeFileUnderRateLimitWithKill moves one file that is far larger
// than memory would like, slowly enough that a kill lands in the middle of it.
// The transfer has to survive the crash, keep what it had verified, and finish
// without the receiver ever holding the file in memory.
func TestScale_LargeFileUnderRateLimitWithKill(t *testing.T) {
	c := transferPair(t)
	a, b := c.Device(0), c.Device(1)

	mib := bigFileMiB(t)
	// The receiver is limited rather than the link: the product has a
	// download limit of its own, and one that the daemon applies is closer
	// to what a user would set than a qdisc. The rate is chosen so the
	// file takes around a minute, which leaves room for a kill in the
	// middle of it. Netem is the fallback when the limit is refused.
	perSecond := mib / 45
	if perSecond < 4 {
		perSecond = 4
	}
	if out, code := b.Run("config", "set", "--download-limit", fmt.Sprintf("%dMiB", perSecond)); code != 0 {
		t.Logf("b refused a download limit (%s), shaping the link instead", strings.TrimSpace(out))
		c.Netem(1, fmt.Sprintf("rate %dmbit", perSecond*8))
	}
	if got := b.Status().Daemon.DownloadLimit; got != int64(perSecond)<<20 {
		t.Logf("b reports a download limit of %d bytes/s", got)
	}

	writeBigFile(t, a, "huge.bin", mib)
	t.Logf("wrote %d MiB on %s, limited to %d MiB/s on %s", mib, a.Name, perSecond, b.Name)

	// The kill lands once the receiver is well into the file.
	var interrupted transferRow
	waitFor(t, "b is 40% through huge.bin", 10*convergeTimeout, func() bool {
		for _, row := range b.Transfers() {
			if row.Path == "huge.bin" && row.ChunksTotal > 0 &&
				row.ChunksDone*10 >= row.ChunksTotal*4 && row.ChunksDone < row.ChunksTotal {
				interrupted = row
				return true
			}
		}
		return false
	})
	startRSS := c.sampleMemory(1).rss
	from := b.LogSize()
	b.KillDaemon()
	recorded := transferRow{}
	for _, row := range b.Transfers() {
		if row.ID == interrupted.ID {
			recorded = row
		}
	}
	if recorded.ChunksDone < interrupted.ChunksDone {
		t.Errorf("b recorded %d chunks of huge.bin, below the %d it had reported",
			recorded.ChunksDone, interrupted.ChunksDone)
	}
	b.StartDaemon()

	waitFor(t, "b picked its transfer up again", daemonTimeout, func() bool {
		return len(resumedChunks(b, from)) > 0
	})
	for _, done := range resumedChunks(b, from) {
		if done < recorded.ChunksDone {
			t.Errorf("b resumed huge.bin with %d chunks, below the %d it had verified",
				done, recorded.ChunksDone)
		}
	}

	// The memory of the receiver is sampled while the rest of the file
	// arrives: a transfer that is streamed costs a few chunks, one that is
	// buffered costs the file. The sampler runs beside the wait and never
	// fails the test itself, because only the test's own goroutine may.
	stop := make(chan struct{})
	samples := make(chan sample)
	go func() {
		peak := sample{rss: startRSS}
		for {
			select {
			case <-stop:
				samples <- peak
				return
			case <-time.After(2 * time.Second):
				now := c.sampleMemory(1)
				if now.rss > peak.rss {
					peak.rss = now.rss
				}
				if now.cgroup > peak.cgroup {
					peak.cgroup = now.cgroup
				}
			}
		}
	}()
	c.WaitConvergedWithin(20 * convergeTimeout)
	close(stop)
	peak := <-samples

	t.Logf("the receiver held at most %.0f MiB of memory while moving %d MiB (its container was charged %.0f MiB, page cache included)",
		peak.rss, mib, peak.cgroup)
	if peak.rss >= 512 {
		t.Errorf("b used %.0f MiB of memory, want less than 512 MiB", peak.rss)
	}

	c.CheckInvariants()
	if got, want := b.Tree()["huge.bin"].Hash, a.Tree()["huge.bin"].Hash; got != want {
		t.Errorf("b holds huge.bin as %s, want the sender's %s", got, want)
	}
}

// writeBigFile puts a file of the given size in a device's folder without ever
// holding it in memory, and puts it in place in one step so no scan sees half
// of it.
func writeBigFile(t *testing.T, dev *Device, rel string, mib int) {
	t.Helper()
	tmp, err := os.CreateTemp(dev.HostRoot, ".e2e-big-")
	if err != nil {
		t.Fatalf("%s: stage %s: %v", dev.Name, rel, err)
	}
	block := make([]byte, 1<<20)
	for i := 0; i < mib; i++ {
		// Every megabyte differs from the last, so a file that arrives
		// with its blocks in the wrong order is a different hash.
		copy(block, payload(len(block), byte(i)))
		if _, err := tmp.Write(block); err != nil {
			t.Fatalf("%s: write %s: %v", dev.Name, rel, err)
		}
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("%s: close %s: %v", dev.Name, rel, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		t.Fatalf("%s: chmod %s: %v", dev.Name, rel, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dev.HostRoot, rel)); err != nil {
		t.Fatalf("%s: put %s in place: %v", dev.Name, rel, err)
	}
}

// sample is what the receiver's memory looked like at one moment: the daemon's
// own resident set and what its container was charged for.
type sample struct {
	rss    float64
	cgroup float64
}

// sampleMemory measures the memory of one device, in mebibytes.
//
// The resident set is read from the kernel rather than from `docker stats`,
// which reports what the container's cgroup is charged for: writing a large
// file through a bind mount charges the cgroup for the page cache holding it,
// so that number follows the size of the file whatever the daemon does (533
// MiB for a 512 MiB transfer the daemon streamed). The cgroup figure is
// sampled too, because it is what an operator watching a container sees.
//
// Nothing here fails the test: it is called from a sampling goroutine, and a
// reading that could not be taken is reported as zero.
func (c *Cluster) sampleMemory(i int) sample {
	dev := c.Devices[i]
	pid, err := os.ReadFile(filepath.Join(dev.HostData, "daemon.pid"))
	if err != nil {
		return sample{}
	}
	var out sample
	res, err := c.d.try("exec", dev.Ctr, "sh", "-c",
		"grep VmRSS /proc/"+strings.TrimSpace(string(pid))+"/status")
	if err == nil && res.code == 0 {
		if fields := strings.Fields(res.stdout); len(fields) >= 2 {
			if kib, err := strconv.ParseFloat(fields[1], 64); err == nil {
				out.rss = kib / 1024
			}
		}
	}
	if res, err := c.d.try("stats", "--no-stream", "--format", "{{.MemUsage}}", dev.Ctr); err == nil && res.code == 0 {
		if used, _, found := strings.Cut(res.stdout, "/"); found {
			out.cgroup = parseSize(strings.TrimSpace(used))
		}
	}
	return out
}

// parseSize turns a docker size such as "45.6MiB" into mebibytes, or zero
// when it is in no unit this test knows.
func parseSize(raw string) float64 {
	units := []struct {
		suffix string
		mib    float64
	}{
		{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"B", 1.0 / (1024 * 1024)},
	}
	for _, unit := range units {
		if !strings.HasSuffix(raw, unit.suffix) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSuffix(raw, unit.suffix), 64)
		if err != nil {
			return 0
		}
		return value * unit.mib
	}
	return 0
}
