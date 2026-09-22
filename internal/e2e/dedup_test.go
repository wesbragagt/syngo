package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// chunkSourceBytes adds up the payload of the chunks a node verified for one path,
// split by where they came from: the peer, over the network, or this device's
// own disk through the chunk index.
//
// It is the measurement the two tests below are about. A chunk taken from the
// local disk crossed no network, so what "bytes moved" means for a transfer is
// exactly fromPeer.
func chunkSourceBytes(t *testing.T, n *node, rel string) (fromPeer, fromLocal int64) {
	t.Helper()
	for _, ev := range n.Events.All() {
		if ev.Kind != "transfer.chunk_verified" {
			continue
		}
		if path, _ := ev.Fields["path"].(string); path != rel {
			continue
		}
		length, ok := ev.Fields["length"].(int)
		if !ok {
			t.Fatalf("chunk_verified event carries no length: %+v", ev.Fields)
		}
		switch source, _ := ev.Fields["source"].(string); source {
		case "peer":
			fromPeer += int64(length)
		case "local":
			fromLocal += int64(length)
		default:
			t.Fatalf("chunk_verified event carries no source: %+v", ev.Fields)
		}
	}
	return fromPeer, fromLocal
}

// localCopyBytes is the other way content already on this device is reused: a
// whole file the plan recognised as a rename or a duplicate of a local file is
// copied into place without a chunk-by-chunk pull at all, so it reports its
// saving once for the file instead of once per chunk.
func localCopyBytes(t *testing.T, n *node, rel string) int64 {
	t.Helper()
	var total int64
	for _, ev := range n.Events.All() {
		if ev.Kind != "transfer.local_copy" {
			continue
		}
		if path, _ := ev.Fields["path"].(string); path != rel {
			continue
		}
		saved, ok := ev.Fields["bytes_saved"].(int64)
		if !ok {
			t.Fatalf("local_copy event carries no bytes_saved: %+v", ev.Fields)
		}
		total += saved
	}
	return total
}

// insertionFileSize is the file the insertion test edits. It is large enough
// that a whole-file transfer is unmistakably more than the edit, and small
// enough that two daemons converge on it twice inside the test timeout.
const insertionFileSize = 8 << 20

// insertionSize is how much is inserted into it.
const insertionSize = 1 << 20

// TestDedup_InsertionMovesOnlyWhatChanged is the point of content-defined
// chunking. A megabyte is inserted near the front of an eight-megabyte file,
// which under fixed-size chunking moves every chunk after it and so the whole
// file. Cutting on content leaves the boundaries after the insertion where
// they were, and the receiver already holds those chunks, so only the edit
// crosses the network.
func TestDedup_InsertionMovesOnlyWhatChanged(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendOnly)
	b := startNode(t, "b", folder.ModeReceiveOnly)

	original := payload(insertionFileSize, 61)
	writeFile(t, a, "big.bin", original)
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	first, firstLocal := chunkSourceBytes(t, b, "big.bin")
	if first < insertionFileSize {
		t.Fatalf("the first copy moved %d bytes of %d, want the whole file", first, insertionFileSize)
	}

	// A megabyte goes in 64 KiB from the start, which is four chunks in at
	// the configured average: everything after it shifts by a megabyte.
	at := 64 << 10
	edited := make([]byte, 0, len(original)+insertionSize)
	edited = append(edited, original[:at]...)
	edited = append(edited, payload(insertionSize, 62)...)
	edited = append(edited, original[at:]...)
	writeFile(t, a, "big.bin", edited)

	waitFor(t, "the edited file to arrive on b", convergeTimeout, func() bool {
		got, err := os.ReadFile(filepath.Join(b.Root, "big.bin"))
		return err == nil && len(got) == len(edited) && string(got) == string(edited)
	})

	total, totalLocal := chunkSourceBytes(t, b, "big.bin")
	moved := total - first
	local := totalLocal - firstLocal
	t.Logf("insertion of %d bytes into %d: %d bytes moved over the network, %d bytes taken from the local copy",
		insertionSize, insertionFileSize, moved, local)

	// The benchmark target for a 1 MiB insertion is under 2 MiB moved.
	if moved >= 2*insertionSize {
		t.Fatalf("%d bytes moved for a %d byte insertion, want less than %d",
			moved, insertionSize, 2*insertionSize)
	}
	if moved >= int64(len(edited))/2 {
		t.Fatalf("%d bytes moved of a %d byte file: the edit cost most of a whole-file transfer",
			moved, len(edited))
	}
	if local <= 0 {
		t.Fatal("nothing was taken from the local copy, so the saving came from somewhere else")
	}
}

// TestDedup_DuplicateFileTransfersNoPayload covers deduplication on its own,
// across files and across folders. A second copy of a file the receiver
// already holds, and a third copy in a different folder, are both built
// entirely from bytes that are already on the receiver's disk: no chunk of
// either is asked for.
func TestDedup_DuplicateFileTransfersNoPayload(t *testing.T) {
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendOnly, ExtraFolder: true})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeReceiveOnly, ExtraFolder: true})

	data := payload(4<<20, 71)
	writeFile(t, a, "original.bin", data)
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	if moved, _ := chunkSourceBytes(t, b, "original.bin"); moved < int64(len(data)) {
		t.Fatalf("the first copy moved %d bytes of %d, want the whole file", moved, len(data))
	}

	// The same bytes again, under another name in the same folder and
	// under a third name in a different folder.
	writeFile(t, a, "copy.bin", data)
	writeFileIn(t, a.Extra, "elsewhere.bin", data)

	waitFor(t, "both duplicates to arrive on b", convergeTimeout, func() bool {
		same := func(p string) bool {
			got, err := os.ReadFile(p)
			return err == nil && string(got) == string(data)
		}
		return same(filepath.Join(b.Root, "copy.bin")) &&
			same(filepath.Join(b.Extra, "elsewhere.bin"))
	})

	for _, rel := range []string{"copy.bin", "elsewhere.bin"} {
		moved, local := chunkSourceBytes(t, b, rel)
		// A duplicate inside one folder is recognised by the plan and
		// copied whole; one in another folder is not, and is built chunk
		// by chunk out of the chunk index. Either way every byte comes
		// from this device.
		local += localCopyBytes(t, b, rel)
		t.Logf("%s: %d bytes moved over the network, %d bytes taken from content already held", rel, moved, local)
		if moved != 0 {
			t.Fatalf("%s moved %d payload bytes, want none: the content was already on this device", rel, moved)
		}
		if local != int64(len(data)) {
			t.Fatalf("%s took %d bytes from local content, want the whole file (%d)", rel, local, len(data))
		}
	}
}
