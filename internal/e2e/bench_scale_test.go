package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// The benchmarks in this file measure the Phase 5 claims that name a ten
// gigabyte file or a hundred thousand files. Each one needs tens of gigabytes
// of disk or minutes of wall time, so they are opt-in exactly as the rest of
// the scale group is: without SYNCTHIS_SCALE=1 they skip, and `make scale`
// runs them. Their measured numbers are recorded in docs/BENCH.md.

const (
	// benchBigGiBEnv sets the size of the file the two big-file
	// benchmarks move, in GiB. The default is the 10 the claims name; a
	// machine with less disk runs a smaller version and reports the size
	// it really ran.
	benchBigGiBEnv = "SYNCTHIS_SCALE_BIGFILE_GIB"
	// benchInsertionSize is the insertion the claim is about.
	benchInsertionSize = 1 << 20
	// benchInsertionOffset is where it goes: near the front, so that under
	// fixed-size chunking every chunk after it would shift and the whole
	// file would move again.
	benchInsertionOffset = 64 << 10
	// benchInsertionBudget is the claim: less than 2 MiB crosses the
	// network for a 1 MiB insertion once the receiver holds the file.
	benchInsertionBudget = 2 << 20
)

// benchBigSize is the size of the file the big-file benchmarks move.
func benchBigSize(t *testing.T) int64 {
	t.Helper()
	return int64(scaleEnvInt(t, benchBigGiBEnv, 10)) << 30
}

// benchStream fills buf with the bytes of a deterministic pseudorandom stream
// starting at absolute offset off.
//
// It is addressable by offset rather than sequential because the insertion
// benchmark has to write the same file again with a megabyte inserted into
// it, without holding ten gigabytes in memory. The bytes are pseudorandom
// rather than a pattern for the reason payload documents: a sequence that
// repeats gives the rolling hash the same window over and over, so the
// chunker cuts nowhere, every chunk is truncated at the maximum size and the
// boundaries come from positions instead of content, which is exactly what an
// insertion would shift.
func benchStream(seed uint64, off int64, buf []byte) {
	for i := range buf {
		at := uint64(off) + uint64(i)
		z := (seed+1)*0x9e3779b97f4a7c15 + at/8*0xff51afd7ed558ccd
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		buf[i] = byte(z >> (8 * (at % 8)))
	}
}

// benchWriteFile writes size bytes into path, drawing each block from gen. It
// writes in blocks so a ten gigabyte file costs four megabytes of memory.
func benchWriteFile(t *testing.T, path string, size int64, gen func(off int64, buf []byte)) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the parent of %s: %v", path, err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	started := time.Now()
	block := make([]byte, 4<<20)
	for written := int64(0); written < size; {
		n := int64(len(block))
		if left := size - written; left < n {
			n = left
		}
		gen(written, block[:n])
		if _, err := file.Write(block[:n]); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		written += n
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	t.Logf("wrote %s of %s in %v", filepath.Base(path), humanBytes(size), time.Since(started))
}

// benchInsertedStream is the original stream with insertSize bytes of a
// second stream inserted at insertAt. Everything after the insertion is the
// original stream again, shifted.
func benchInsertedStream(seed, insertSeed uint64, insertAt, insertSize int64) func(int64, []byte) {
	return func(off int64, buf []byte) {
		for i := int64(0); i < int64(len(buf)); {
			at := off + i
			var n int64
			switch {
			case at < insertAt:
				n = min(insertAt-at, int64(len(buf))-i)
				benchStream(seed, at, buf[i:i+n])
			case at < insertAt+insertSize:
				n = min(insertAt+insertSize-at, int64(len(buf))-i)
				benchStream(insertSeed, at-insertAt, buf[i:i+n])
			default:
				n = int64(len(buf)) - i
				benchStream(seed, at-insertSize, buf[i:i+n])
			}
			i += n
		}
	}
}

// benchBigPair starts two in-process daemons with the shipped timings and the
// shipped chunk size, their folder roots in dir and their data directories in
// a short path, because a data directory holds a unix socket and dir is deep
// inside a checkout. They are paired and their folders converged.
func benchBigPair(t *testing.T, dir string) (a, b *node) {
	t.Helper()
	short := scaleShortDir(t)
	configA := benchNodeConfig(folder.ModeSendReceive)
	configA.DataDir, configA.Root = filepath.Join(short, "a"), filepath.Join(dir, "a-root")
	configB := benchNodeConfig(folder.ModeSendReceive)
	configB.DataDir, configB.Root = filepath.Join(short, "b"), filepath.Join(dir, "b-root")

	a = startNodeWith(t, "a", configA)
	b = startNodeWith(t, "b", configB)
	pair(t, a, b)
	benchReload(t, a, b)
	return a, b
}

// benchBigTimeout is how long a ten gigabyte transfer may take. It is
// generous on purpose: the benchmark reports the time it took, and a bound
// tight enough to be a claim about throughput would fail on a slower disk for
// no reason worth acting on.
var benchBigTimeout = 60 * time.Minute * timeoutScale

// benchPublish writes a file into a staging directory on the same filesystem
// and renames it into the folder root, so the daemon never scans a file that
// is still being written.
func benchPublish(t *testing.T, dir, root, name string, size int64, gen func(int64, []byte)) {
	t.Helper()
	staged := filepath.Join(dir, "staging", name)
	benchWriteFile(t, staged, size, gen)
	publish(t, staged, root, name)
}

// TestScale_BenchBigFileInsertion is the Phase 5 claim that a megabyte
// inserted into a ten gigabyte file moves less than two megabytes once the
// receiver holds the file.
//
// It fails today, and the assertion is left at the claim rather than at what
// the machine measures, the same way the idle memory test is. At the shipped
// 1 MiB average chunk size an insertion re-cuts the chunks it lands in as
// well as carrying its own megabyte, so the cost is the insertion plus a
// chunk or so, which is above the two megabyte budget whatever the size of
// the file: 2,254,262 bytes in the run recorded in docs/BENCH.md. The
// budget holds at smaller chunk sizes, which is what
// TestDedup_InsertionMovesOnlyWhatChanged measures at 16 KiB.
func TestScale_BenchBigFileInsertion(t *testing.T) {
	requireScale(t)

	size := benchBigSize(t)
	dir := scaleScratch(t)
	// The sender's copy, the receiver's copy, the staged copy and the
	// receiver's partial download each hold the file, and the sender holds
	// both revisions for as long as the staged one exists. A run that
	// would fill the disk is refused before it starts.
	if free, err := freeBytes(dir); err == nil && free < 5*size {
		t.Skipf("an insertion benchmark over %s needs about %s free under %s and there is %s",
			humanBytes(size), humanBytes(5*size), scaleDirEnv, humanBytes(free))
	}

	a, b := benchBigPair(t, dir)
	benchPublish(t, dir, a.Root, "big.bin", size, func(off int64, buf []byte) {
		benchStream(1, off, buf)
	})

	arrived := filepath.Join(b.Root, "big.bin")
	started := time.Now()
	waitFor(t, "the first copy to arrive", benchBigTimeout, func() bool {
		return sizeOf(arrived) == size && len(partFiles(t, b.Root)) == 0
	})
	t.Logf("the first copy of %s arrived in %v", humanBytes(size), time.Since(started))

	first, _ := chunkSourceBytes(t, b, "big.bin")
	if first < size {
		t.Fatalf("the first copy moved %d bytes of %d, want the whole file", first, size)
	}

	// The insertion: a megabyte near the front, so every later boundary
	// would shift under fixed-size chunking.
	edited := size + benchInsertionSize
	benchPublish(t, dir, a.Root, "big.bin", edited,
		benchInsertedStream(1, 2, benchInsertionOffset, benchInsertionSize))
	want := hashOfFile(t, filepath.Join(a.Root, "big.bin"))

	started = time.Now()
	waitFor(t, "the edited copy to arrive", benchBigTimeout, func() bool {
		return sizeOf(arrived) == edited && len(partFiles(t, b.Root)) == 0
	})
	elapsed := time.Since(started)
	if got := hashOfFile(t, arrived); got != want {
		t.Fatalf("the edited file hashes to %x on b, want %x", got, want)
	}

	total, totalLocal := chunkSourceBytes(t, b, "big.bin")
	moved := total - first
	t.Logf("a %s insertion into a %s file: %s (%d bytes) moved over the network, %s taken from "+
		"the copy already on disk, in %v (budget %s, %d bytes)",
		humanBytes(benchInsertionSize), humanBytes(size), humanBytes(moved), moved,
		humanBytes(totalLocal), elapsed, humanBytes(benchInsertionBudget), benchInsertionBudget)

	if moved >= benchInsertionBudget {
		t.Fatalf("the insertion moved %s, want less than %s",
			humanBytes(moved), humanBytes(benchInsertionBudget))
	}
}

// TestScale_BenchBigFileRename is the Phase 5 claim that renaming a ten
// gigabyte file costs nothing on the wire.
func TestScale_BenchBigFileRename(t *testing.T) {
	requireScale(t)

	size := benchBigSize(t)
	dir := scaleScratch(t)
	if free, err := freeBytes(dir); err == nil && free < 3*size {
		t.Skipf("a rename benchmark over %s needs about %s free under %s and there is %s",
			humanBytes(size), humanBytes(3*size), scaleDirEnv, humanBytes(free))
	}

	a, b := benchBigPair(t, dir)
	benchPublish(t, dir, a.Root, "big.bin", size, func(off int64, buf []byte) {
		benchStream(3, off, buf)
	})

	arrived := filepath.Join(b.Root, "big.bin")
	started := time.Now()
	waitFor(t, "the first copy to arrive", benchBigTimeout, func() bool {
		return sizeOf(arrived) == size && len(partFiles(t, b.Root)) == 0
	})
	t.Logf("the first copy of %s arrived in %v", humanBytes(size), time.Since(started))

	first, _ := chunkSourceBytes(t, b, "big.bin")
	if first < size {
		t.Fatalf("the first copy moved %d bytes of %d, want the whole file", first, size)
	}
	want := hashOfFile(t, filepath.Join(a.Root, "big.bin"))

	if err := os.Rename(filepath.Join(a.Root, "big.bin"), filepath.Join(a.Root, "moved.bin")); err != nil {
		t.Fatalf("rename on a: %v", err)
	}
	moved := filepath.Join(b.Root, "moved.bin")
	started = time.Now()
	waitFor(t, "the renamed file to appear on b", benchBigTimeout, func() bool {
		return sizeOf(moved) == size && len(partFiles(t, b.Root)) == 0
	})
	waitFor(t, "the old path to go away on b", benchBigTimeout, func() bool {
		_, err := os.Stat(arrived)
		return os.IsNotExist(err)
	})
	elapsed := time.Since(started)
	if got := hashOfFile(t, moved); got != want {
		t.Fatalf("the renamed file hashes to %x on b, want %x", got, want)
	}

	fromPeer, fromLocal := chunkSourceBytes(t, b, "moved.bin")
	fromLocal += localCopyBytes(t, b, "moved.bin")
	t.Logf("renaming %s: %d bytes moved over the network, %s taken from the copy already on disk, in %v",
		humanBytes(size), fromPeer, humanBytes(fromLocal), elapsed)

	if fromPeer != 0 {
		t.Fatalf("the rename moved %s of payload, want none", humanBytes(fromPeer))
	}
	if fromLocal != size {
		t.Fatalf("the rename took %s from the local copy, want the whole file (%s)",
			humanBytes(fromLocal), humanBytes(size))
	}
}

const (
	// benchStatusTarget is the Phase 5 claim: status answers in under two
	// seconds over a 100,000-file folder that has just changed.
	benchStatusTarget = 2 * time.Second
	// benchStatusCalls is how many times status is run once the folder has
	// settled. One call measures whatever the machine was doing at that
	// instant.
	benchStatusCalls = 10
	// benchStatusChangedDirs is the incremental change: this many
	// directories of the tree are rewritten, which at the default shape is
	// 1,000 of the 100,000 files.
	benchStatusChangedDirs = 2
)

// TestScale_BenchStatusLatency measures how long `syncthis status` takes over
// a 100,000-file folder right after an incremental change, which is the
// moment it is slowest: the daemon is scanning what changed while the command
// asks it what it holds.
//
// It measures the command, not an internal call: what the claim is about is
// what an operator waits for, which includes starting the process and
// round-tripping the control socket.
func TestScale_BenchStatusLatency(t *testing.T) {
	requireScale(t)
	requireBinary(t)

	filesEach := scaleEnvInt(t, scaleTreeFilesEachEnv, scaleTreeFilesEachFull)
	dir := scaleScratch(t)
	root := filepath.Join(dir, "root")
	tree := buildScaleTree(t, root, scaleTreeDirs, filesEach)

	dataDir := filepath.Join(scaleShortDir(t), "data")
	if code, out := runBinary(t, dataDir, "init", "--name", "status", "--json"); code != 0 {
		t.Fatalf("init exit = %d, want 0 (%s)", code, out)
	}
	if code, out := runBinary(t, dataDir, "folder", "add", root, "--id", string(folderID), "--json"); code != 0 {
		t.Fatalf("folder add exit = %d, want 0 (%s)", code, out)
	}

	daemon := startChild(t, dataDir, "daemon", "run", "--listen", "127.0.0.1:0")
	scanTimeout := 10 * time.Minute * timeoutScale
	waitFor(t, "the daemon to scan the whole folder and go idle", scanTimeout, func() bool {
		if daemon.finished() {
			t.Fatalf("the daemon exited: %s", daemon.errorOutput())
		}
		report := statusJSON(t, dataDir)
		if !report.Daemon.Running || len(report.Folders) != 1 {
			return false
		}
		f := report.Folders[0]
		return f.Files == int64(tree.files) && f.State == "idle" && f.PendingChanges == 0
	})

	// The incremental change, and with it the two numbers this benchmark
	// reports: how long a status call takes while the daemon is taking the
	// change in, and how long it is before status reports the change.
	changed := 0
	for d := range benchStatusChangedDirs {
		for f := range filesEach {
			tree.writeScaleFile(t, d, f, 1, filesEach)
			changed++
		}
	}
	changedAt := time.Now()

	var during []time.Duration
	var reflected time.Duration
	for {
		started := time.Now()
		code, out := runBinary(t, dataDir, "status", "--json")
		during = append(during, time.Since(started))
		if code != 0 {
			t.Fatalf("status exit = %d, want 0 (%s)", code, out)
		}
		report := statusJSON(t, dataDir)
		if report.Folders[0].Bytes == tree.bytes {
			reflected = time.Since(changedAt)
			break
		}
		if time.Since(changedAt) > scanTimeout {
			t.Fatalf("status never reported the %d changed files", changed)
		}
	}

	var after []time.Duration
	for range benchStatusCalls {
		started := time.Now()
		if code, out := runBinary(t, dataDir, "status", "--json"); code != 0 {
			t.Fatalf("status exit = %d, want 0 (%s)", code, out)
		}
		after = append(after, time.Since(started))
	}

	slowestDuring := slices.Max(during)
	slowestAfter := slices.Max(after)
	sorted := slices.Clone(after)
	slices.Sort(sorted)
	t.Logf("%d files, %d of them rewritten: %d status calls while the daemon took the change in, "+
		"slowest %v; the change was reported %v after it was made; %d calls afterwards, "+
		"median %v, slowest %v (target %v)",
		tree.files, changed, len(during), slowestDuring, reflected,
		len(after), sorted[len(sorted)/2], slowestAfter, benchStatusTarget)

	if slowestDuring >= benchStatusTarget || slowestAfter >= benchStatusTarget {
		t.Fatalf("status took up to %v while the change was taken in and up to %v afterwards, "+
			"want every call under %v", slowestDuring, slowestAfter, benchStatusTarget)
	}
}
