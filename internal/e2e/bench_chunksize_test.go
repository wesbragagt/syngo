package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// The two tests in this file are the measurement behind the Phase 5 decision
// on DefaultChunkSize: a smaller average chunk closes the 2 MiB insertion
// budget, and the question is what it costs an ordinary large-file transfer
// that has nothing to deduplicate. Neither number can be argued from a
// formula alone, so both sides are measured at the same set of averages over
// the same file, and the result is recorded in docs/BENCH.md.
//
// They are opt-in with the rest of the scale group. Each one moves the file
// once per average, so at the default of four averages and 1 GiB they move
// about 4 GiB (the insertion sweep moves it twice per average).

const (
	// chunkEvalAveragesEnv is the list of average chunk sizes to measure,
	// in KiB, largest first.
	chunkEvalAveragesEnv = "SYNCTHIS_SCALE_CHUNKEVAL_KIB"
	// chunkEvalSizeEnv is the size of the file each average moves, in MiB.
	// One gibibyte is enough: the insertion cost is a function of the
	// insertion and the chunk size and not of the file, which the 1 GiB
	// and 10 GiB runs in docs/BENCH.md agree on.
	chunkEvalSizeEnv = "SYNCTHIS_SCALE_CHUNKEVAL_MIB"
	// chunkEvalOffsetEnv is where the megabyte is inserted, in KiB from
	// the start. The cost of an insertion is the insertion plus the chunks
	// its two ends land in, so it depends on where the boundaries around
	// that position happen to fall: an average whose margin against the
	// budget is small at one position is not a safe default, and moving
	// the position is how that is found out.
	chunkEvalOffsetEnv = "SYNCTHIS_SCALE_CHUNKEVAL_OFFSET_KIB"
)

// chunkEvalAverages returns the average chunk sizes under test.
func chunkEvalAverages(t *testing.T) []int {
	t.Helper()
	raw := os.Getenv(chunkEvalAveragesEnv)
	if raw == "" {
		return []int{1024 << 10, 256 << 10, 64 << 10, 16 << 10}
	}
	var out []int
	for _, field := range strings.Split(raw, ",") {
		kib, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || kib < 4 {
			t.Fatalf("%s = %q, want a comma separated list of sizes in KiB of at least 4",
				chunkEvalAveragesEnv, raw)
		}
		out = append(out, kib<<10)
	}
	return out
}

// chunkEvalSize is the size of the file the sweeps move.
func chunkEvalSize(t *testing.T) int64 {
	t.Helper()
	return int64(scaleEnvInt(t, chunkEvalSizeEnv, 1024)) << 20
}

// chunkEvalOffset is where the sweep inserts its megabyte.
func chunkEvalOffset(t *testing.T) int64 {
	t.Helper()
	return int64(scaleEnvInt(t, chunkEvalOffsetEnv, benchInsertionOffset>>10)) << 10
}

// databaseBytes is how much disk a node's database occupies: the file and its
// write-ahead log, which is where rows written since the last checkpoint are.
//
// A reading taken while the daemon runs counts an uncheckpointed log, which
// holds every version of a page written since the last checkpoint and so says
// more about how the writes were spaced than about how much index there is.
// The index figures in docs/BENCH.md are read after the node is stopped,
// which checkpoints the log into the file.
func databaseBytes(t *testing.T, dataDir string) int64 {
	t.Helper()
	var total int64
	for _, name := range []string{compose.DatabaseFile, compose.DatabaseFile + "-wal"} {
		if size := sizeOf(filepath.Join(dataDir, name)); size > 0 {
			total += size
		}
	}
	return total
}

// waitIndexed waits until a node's chunk index covers a path and returns how
// many chunks it holds for it. The index is written as the file is installed
// and by the scan that follows it, so a reading taken the moment the bytes
// land can find nothing yet.
func waitIndexed(t *testing.T, n *node, path string) int {
	t.Helper()
	var chunks int
	waitFor(t, "the chunk index of "+path+" on "+n.name, convergeTimeout, func() bool {
		found, err := n.App.Store.FileChunks(context.Background(), folderID, path)
		chunks = len(found.Chunks)
		return err == nil && chunks > 0
	})
	return chunks
}

// TestScale_BenchChunkSizeInsertionSweep measures both sides of the chunk size
// tradeoff at each average: what a 1 MiB insertion into a large file costs on
// the wire, and what the ordinary first transfer of that file costs in time,
// in chunks and in chunk index rows.
//
// It asserts nothing about a particular average. The claim under test,
// "an insertion moves less than 2 MiB", is asserted by
// TestScale_BenchBigFileInsertion at whatever the shipped default is; this
// test is the evidence for choosing that default, so it reports every number
// and fails only when a measurement could not be taken.
func TestScale_BenchChunkSizeInsertionSweep(t *testing.T) {
	requireScale(t)

	size := chunkEvalSize(t)
	offset := chunkEvalOffset(t)
	averages := chunkEvalAverages(t)
	for _, avg := range averages {
		t.Run(humanBytes(int64(avg)), func(t *testing.T) {
			dir := scaleScratch(t)
			if free, err := freeBytes(dir); err == nil && free < 5*size {
				t.Skipf("the sweep over %s needs about %s free under %s and there is %s",
					humanBytes(size), humanBytes(5*size), scaleDirEnv, humanBytes(free))
			}

			short := scaleShortDir(t)
			configA := benchNodeConfig(folder.ModeSendReceive)
			configA.ChunkSize = avg
			configA.DataDir, configA.Root = filepath.Join(short, "a"), filepath.Join(dir, "a-root")
			configB := benchNodeConfig(folder.ModeSendReceive)
			configB.ChunkSize = avg
			configB.DataDir, configB.Root = filepath.Join(short, "b"), filepath.Join(dir, "b-root")
			a := startNodeWith(t, "a", configA)
			b := startNodeWith(t, "b", configB)
			pair(t, a, b)
			benchReload(t, a, b)

			emptyDB := databaseBytes(t, b.DataDir)
			benchPublish(t, dir, a.Root, "big.bin", size, func(off int64, buf []byte) {
				benchStream(1, off, buf)
			})

			arrived := filepath.Join(b.Root, "big.bin")
			started := time.Now()
			waitFor(t, "the first copy to arrive", benchBigTimeout, func() bool {
				return sizeOf(arrived) == size && len(partFiles(t, b.Root)) == 0
			})
			firstElapsed := time.Since(started)

			first, _ := chunkSourceBytes(t, b, "big.bin")
			if first < size {
				t.Fatalf("the first copy moved %d bytes of %d, want the whole file", first, size)
			}
			chunks := waitIndexed(t, b, "big.bin")

			// The insertion: a megabyte near the front, so every later
			// boundary would shift under fixed-size chunking.
			edited := size + benchInsertionSize
			benchPublish(t, dir, a.Root, "big.bin", edited,
				benchInsertedStream(1, 2, offset, benchInsertionSize))
			want := hashOfFile(t, filepath.Join(a.Root, "big.bin"))

			started = time.Now()
			waitFor(t, "the edited copy to arrive", benchBigTimeout, func() bool {
				return sizeOf(arrived) == edited && len(partFiles(t, b.Root)) == 0
			})
			editElapsed := time.Since(started)
			if got := hashOfFile(t, arrived); got != want {
				t.Fatalf("the edited file hashes to %x on b, want %x", got, want)
			}

			total, _ := chunkSourceBytes(t, b, "big.bin")
			moved := total - first
			editedChunks := waitIndexed(t, b, "big.bin")

			// The index cost is read with the database closed, so the
			// write-ahead log is checkpointed into the file and the
			// figure is the index rather than the writing of it.
			b.Stop()
			indexDB := databaseBytes(t, b.DataDir) - emptyDB

			verdict := "within"
			if moved >= benchInsertionBudget {
				verdict = "over"
			}
			t.Logf("average %s over %s, insertion at %s: first copy in %v (%s/s), %d chunks (%d bytes each on average), "+
				"database %s over %d indexed chunks (%d bytes per chunk, %.3f%% of the content); "+
				"a %s insertion moved %s (%d bytes) in %v, %s the %s budget",
				humanBytes(int64(avg)), humanBytes(size), humanBytes(offset), firstElapsed,
				humanBytes(int64(float64(size)/firstElapsed.Seconds())),
				chunks, size/int64(max(chunks, 1)),
				humanBytes(indexDB), editedChunks, indexDB/int64(max(editedChunks, 1)),
				100*float64(indexDB)/float64(size),
				humanBytes(benchInsertionSize), humanBytes(moved), moved, editElapsed,
				verdict, humanBytes(benchInsertionBudget))
		})
	}
}

// TestScale_BenchChunkSizeTransferCost measures what a smaller average costs
// the daemon processes themselves on an ordinary large-file transfer with
// nothing to deduplicate: wall time, throughput and peak resident memory on
// both sides.
//
// It drives two real daemon processes rather than two daemons inside the test
// binary, for the reason the rest of the scale transfer group does: a memory
// reading of "the daemon" must not include the test that generated the file.
func TestScale_BenchChunkSizeTransferCost(t *testing.T) {
	requireScale(t)
	requireProcMemory(t)
	requireBinary(t)

	size := chunkEvalSize(t)
	for _, avg := range chunkEvalAverages(t) {
		t.Run(humanBytes(int64(avg)), func(t *testing.T) {
			dir := scaleScratch(t)
			if free, err := freeBytes(dir); err == nil && free < 3*size {
				t.Skipf("a %s transfer needs about %s free under %s and there is %s",
					humanBytes(size), humanBytes(3*size), scaleDirEnv, humanBytes(free))
			}

			a, b := startScalePair(t, dir, scalePairConfig{
				ChunkSize:      avg,
				RescanInterval: 5 * time.Second,
				SyncInterval:   time.Second,
			})
			staged, want := stageFile(t, filepath.Join(dir, "staging"), "big.bin", size)
			senderSampler := sampleRSS(a.PID(), 250*time.Millisecond)
			receiverSampler := sampleRSS(b.PID(), 250*time.Millisecond)

			started := time.Now()
			publish(t, staged, a.root, "big.bin")
			arrived := filepath.Join(b.root, "big.bin")
			waitFor(t, "the file to arrive complete", benchBigTimeout, func() bool {
				if a.child.finished() || b.child.finished() {
					t.Fatalf("a daemon exited during the transfer: %s%s",
						a.child.errorOutput(), b.child.errorOutput())
				}
				return sizeOf(arrived) == size && len(b.partialFiles()) == 0
			})
			elapsed := time.Since(started)

			senderPeak, senderSamples, senderErr := senderSampler.Stop()
			receiverPeak, receiverSamples, receiverErr := receiverSampler.Stop()
			if senderErr != nil || receiverErr != nil || senderSamples == 0 || receiverSamples == 0 {
				t.Fatalf("could not sample the daemons' memory: %v, %v (%d, %d samples)",
					senderErr, receiverErr, senderSamples, receiverSamples)
			}
			if got := hashOfFile(t, arrived); got != want {
				t.Fatalf("the file that arrived hashes to %x, want %x", got, want)
			}
			assertNoBrokenTransfers(t, b)
			assertNoPartialFiles(t, b)

			kernel := func(n *scaleNode) string {
				peak, err := peakResidentBytes(n.PID())
				if err != nil {
					return "unreadable"
				}
				return humanBytes(peak)
			}
			t.Logf("average %s over %s: arrived in %v (%s/s); peak resident memory sender %s, "+
				"receiver %s over %d and %d samples; kernel peak sender %s, receiver %s; "+
				"receiver database %s",
				humanBytes(int64(avg)), humanBytes(size), elapsed,
				humanBytes(int64(float64(size)/elapsed.Seconds())),
				humanBytes(senderPeak), humanBytes(receiverPeak), senderSamples, receiverSamples,
				kernel(a), kernel(b), humanBytes(databaseBytes(t, b.dataDir)))
		})
	}
}
