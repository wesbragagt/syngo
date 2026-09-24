package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// The mirror test is the Phase 3 claim that a vault-sized folder mirrors and
// then survives a restart of both daemons without any of it crossing the
// network a second time.
//
// The size is the vault-scale target of adr-006: the folder this program is
// being built to carry is a notes vault, a few thousand markdown files plus
// attachments, well under a gigabyte today. 2 GiB is generously larger than
// that with the attachments an image- and PDF-heavy vault accumulates, and it
// is the number the task list names. Every parameter is overridable so a
// machine with more disk runs a larger version of the same measurement and
// reports what it really ran.
const (
	// mirrorMiBEnv is the total size of the mirrored folder, in MiB.
	mirrorMiBEnv = "SYNGO_SCALE_MIRROR_MIB"
	// mirrorNotesEnv is how many note-sized files the folder holds.
	mirrorNotesEnv = "SYNGO_SCALE_MIRROR_NOTES"
	// mirrorAttachmentSize is the size of one attachment. A vault's
	// largest files are images, PDFs and the occasional audio note: low
	// tens of megabytes, not gigabytes.
	mirrorAttachmentSize = 32 << 20
	// mirrorNoteSize is the average size of a note. Consecutive notes
	// differ in length around it, for the reason scaleFileContent
	// documents.
	mirrorNoteSize = 12 << 10
)

// mirrorSettle is how long both daemons are watched after they come back up.
// It has to be longer than the shipped sync interval, or a retransfer that
// the periodic cycle would have started has simply not been given the chance
// to happen yet and the test proves nothing.
const mirrorSettleIntervals = 3

// buildVaultTree writes a notes vault into root: many note-sized markdown
// files across a handful of directories, plus attachments large enough that a
// retransfer of one is unmistakable. It returns how many files and bytes it
// wrote.
func buildVaultTree(t *testing.T, root string, notes int, total int64) (files int, bytes int64) {
	t.Helper()
	started := time.Now()
	const noteDirs = 20
	for d := range noteDirs {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("notes/%02d", d)), 0o755); err != nil {
			t.Fatalf("create a note directory: %v", err)
		}
	}
	for n := range notes {
		// The length varies with the index, so no two consecutive
		// notes are the same size and no two notes hold the same
		// bytes: identical content would be deduplicated and the
		// mirror would move less than the folder holds, which is a
		// real property of the program but not what this test claims.
		size := int64(mirrorNoteSize + (n%97)*128)
		path := filepath.Join(root, fmt.Sprintf("notes/%02d/note-%05d.md", n%noteDirs, n))
		benchWriteQuiet(t, path, size, func(off int64, buf []byte) {
			benchStream(uint64(n)+1, off, buf)
		})
		files++
		bytes += size
	}
	attachments := 0
	for bytes < total {
		size := int64(mirrorAttachmentSize)
		if left := total - bytes; left < size {
			size = left
		}
		path := filepath.Join(root, fmt.Sprintf("attachments/att-%03d.bin", attachments))
		benchWriteQuiet(t, path, size, func(off int64, buf []byte) {
			benchStream(uint64(attachments)+9001, off, buf)
		})
		attachments++
		files++
		bytes += size
	}
	t.Logf("built a vault of %d files (%d notes, %d attachments of %s) totalling %s in %v",
		files, notes, attachments, humanBytes(mirrorAttachmentSize), humanBytes(bytes),
		time.Since(started))
	return files, bytes
}

// benchWriteQuiet is benchWriteFile without the per-file log line, for a
// fixture of thousands of files.
func benchWriteQuiet(t *testing.T, path string, size int64, gen func(off int64, buf []byte)) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the parent of %s: %v", path, err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	block := make([]byte, 1<<20)
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
}

// peerChunkBytes is how many payload bytes a node verified from its peer,
// across every path. A node's event recorder is replaced when it restarts, so
// reading this after a restart counts only what the restarted daemon pulled.
func peerChunkBytes(t *testing.T, n *node) int64 {
	t.Helper()
	var total int64
	for _, ev := range n.Events.All() {
		if ev.Kind != "transfer.chunk_verified" {
			continue
		}
		if source, _ := ev.Fields["source"].(string); source != "peer" {
			continue
		}
		length, ok := ev.Fields["length"].(int)
		if !ok {
			t.Fatalf("chunk_verified event carries no length: %+v", ev.Fields)
		}
		total += int64(length)
	}
	return total
}

// TestScale_VaultMirrorSurvivesRestart mirrors a vault-sized folder from a
// send-only device to a receive-only one, restarts both daemons, and checks
// that the receiver pulls nothing at all afterwards: the mirror resumes from
// what is already on disk rather than starting the folder over.
func TestScale_VaultMirrorSurvivesRestart(t *testing.T) {
	requireScale(t)

	total := int64(scaleEnvInt(t, mirrorMiBEnv, 2048)) << 20
	notes := scaleEnvInt(t, mirrorNotesEnv, 4000)
	dir := scaleScratch(t)
	// The sender's copy and the receiver's copy, plus room for the
	// receiver's partial downloads. A run that would fill the disk is
	// refused before it starts rather than failing on a write error.
	if free, err := freeBytes(dir); err == nil && free < 3*total {
		t.Skipf("a %s mirror needs about %s free under %s and there is %s",
			humanBytes(total), humanBytes(3*total), scaleDirEnv, humanBytes(free))
	}

	short := scaleShortDir(t)
	configA := benchNodeConfig(folder.ModeSendOnly)
	configA.DataDir, configA.Root = filepath.Join(short, "a"), filepath.Join(dir, "a-root")
	configB := benchNodeConfig(folder.ModeReceiveOnly)
	configB.DataDir, configB.Root = filepath.Join(short, "b"), filepath.Join(dir, "b-root")

	a := startNodeWith(t, "a", configA)
	b := startNodeWith(t, "b", configB)
	files, bytes := buildVaultTree(t, a.Root, notes, total)
	pair(t, a, b)
	benchReload(t, a, b)

	mirrorTimeout := 60 * time.Minute * timeoutScale
	started := time.Now()
	waitFor(t, "the whole vault to arrive on the receiver", mirrorTimeout, func() bool {
		report := b.Status()
		if len(report.Folders) != 1 || report.Folders[0].Files != int64(files) {
			return false
		}
		return report.Transfers.Active == 0 && report.Transfers.Pending == 0 &&
			len(partFiles(t, b.Root)) == 0
	})
	elapsed := time.Since(started)
	if problems := treeDiff(t, a.Root, b.Root); len(problems) != 0 {
		t.Fatalf("the mirror does not match the source:\n%s", join(problems))
	}
	firstPass := peerChunkBytes(t, b)
	t.Logf("mirrored %d files (%s) in %v; the receiver pulled %s from the peer",
		files, humanBytes(bytes), elapsed, humanBytes(firstPass))
	if firstPass < bytes/2 {
		t.Fatalf("the first mirror pulled only %s of %s from the peer; the fixture "+
			"deduplicated and this run does not measure a full mirror",
			humanBytes(firstPass), humanBytes(bytes))
	}

	// The restart the claim is about: both daemons stop and start again
	// from the same data directories, with fresh event recorders, so
	// everything counted below was pulled by the restarted daemon.
	a.Restart()
	b.Restart()

	// Give both of them long enough that a periodic sync cycle, not only
	// the one at startup, would have re-fetched the folder had it decided
	// to. Anything less measures a daemon that has not yet had the chance
	// to retransfer.
	settle := mirrorSettleIntervals * benchNodeConfig(folder.ModeSendOnly).SyncInterval
	deadline := time.Now().Add(settle)
	for time.Now().Before(deadline) {
		if pulled := peerChunkBytes(t, b); pulled != 0 {
			t.Fatalf("the receiver pulled %s from the peer after the restart, want none",
				humanBytes(pulled))
		}
		time.Sleep(time.Second)
	}

	after := peerChunkBytes(t, b)
	t.Logf("after restarting both daemons and waiting %v (%d sync intervals of %v): "+
		"the receiver pulled %d payload bytes, %d transfers active, %d pending",
		settle, mirrorSettleIntervals, benchNodeConfig(folder.ModeSendOnly).SyncInterval,
		after, b.Status().Transfers.Active, b.Status().Transfers.Pending)

	if after != 0 {
		t.Fatalf("the receiver pulled %s after the restart, want a mirror that resumes "+
			"from what is on disk", humanBytes(after))
	}
	if problems := treeDiff(t, a.Root, b.Root); len(problems) != 0 {
		t.Fatalf("the mirror diverged from the source over the restart:\n%s", join(problems))
	}
	if parts := partFiles(t, b.Root); len(parts) != 0 {
		t.Fatalf("the receiver left partial downloads behind: %v", parts)
	}
}
