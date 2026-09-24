package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// renameSize is the file the rename test moves. It is large enough that the
// first transfer takes hundreds of chunks, so "zero chunks the second time" is
// a measurement and not a rounding error.
const renameSize = 8 << 20

// chunkBytes returns how many payload bytes a node verified for one path,
// derived from the chunk events it recorded. Every chunk but the last is a
// full one, which is what makes the count a byte figure.
func chunkBytes(events []ports.Event, path string, chunkSize int) (chunks int, bytes int64) {
	for _, ev := range events {
		if ev.Kind != "transfer.chunk_verified" || ev.Fields["path"] != path {
			continue
		}
		chunks++
	}
	return chunks, int64(chunks) * int64(chunkSize)
}

// TestRename_LargeFileTransfersZeroPayloadBytes is the Phase 5 claim: a file
// the peer renamed is built from the copy this device already has, so the
// rename costs nothing on the wire.
func TestRename_LargeFileTransfersZeroPayloadBytes(t *testing.T) {
	a := startNode(t, "a", folder.ModeSendReceive)
	b := startNode(t, "b", folder.ModeSendReceive)
	pair(t, a, b)

	data := payload(renameSize, 53)
	writeFile(t, a, "big.bin", data)
	waitFileEqual(t, b, "big.bin", data)

	chunks, bytes := chunkBytes(b.Events.All(), "big.bin", testChunkSize)
	if chunks == 0 {
		t.Fatal("the first transfer verified no chunk, so the rename proves nothing")
	}
	t.Logf("first transfer of %d bytes: %d chunks, %d payload bytes", len(data), chunks, bytes)

	if err := os.Rename(filepath.Join(a.Root, "big.bin"), filepath.Join(a.Root, "moved.bin")); err != nil {
		t.Fatalf("rename on a: %v", err)
	}
	waitFileEqual(t, b, "moved.bin", data)
	waitFor(t, "big.bin to go away on b", convergeTimeout, func() bool {
		_, err := os.Stat(filepath.Join(b.Root, "big.bin"))
		return os.IsNotExist(err)
	})

	movedChunks, movedBytes := chunkBytes(b.Events.All(), "moved.bin", testChunkSize)
	if movedChunks != 0 {
		t.Fatalf("the rename cost %d chunks and %d payload bytes, want 0", movedChunks, movedBytes)
	}
	if b.Events.Count("transfer.local_copy") == 0 {
		t.Fatal("the file arrived without a local copy event, so it was not taken from the old path")
	}
	t.Logf("rename of %d bytes: %d chunks, %d payload bytes", len(data), movedChunks, movedBytes)

	waitTreeEqual(t, a, b)
}
