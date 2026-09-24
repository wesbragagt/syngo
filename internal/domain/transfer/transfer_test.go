package transfer_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/transfer"
)

func hashOf(seed byte) manifest.Hash {
	var h manifest.Hash
	for i := range h {
		h[i] = seed
	}
	return h
}

// fixedChunks cuts a file of the given size every chunkSize bytes. The
// chunker cuts on content; these tests are about what a transfer does with a
// chunk list, so they use a layout that is easy to state.
func fixedChunks(size int64, chunkSize int) []transfer.Chunk {
	var out []transfer.Chunk
	for offset := int64(0); offset < size; offset += int64(chunkSize) {
		length := int64(chunkSize)
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		out = append(out, transfer.Chunk{Index: len(out), Offset: offset, Length: int(length)})
	}
	return out
}

func newTransfer(t *testing.T, size int64, chunkSize int) transfer.Transfer {
	t.Helper()
	entry := manifest.Entry{
		Path:    "big.iso",
		Kind:    manifest.KindFile,
		Size:    size,
		Hash:    hashOf(1),
		Mode:    0o644,
		ModTime: time.Unix(1700000000, 0).UTC(),
	}
	now := time.Unix(1700000001, 0).UTC()
	chunks := fixedChunks(size, max(chunkSize, 1))
	return transfer.NewInbound("abc", "docs", entry, "PEER", chunks, chunkSize, "/tmp/abc.part", now)
}

func TestValidateChunks(t *testing.T) {
	tests := []struct {
		name    string
		size    int64
		chunks  []transfer.Chunk
		wantErr bool
	}{
		{"empty file has no chunks", 0, nil, false},
		{"a whole file in one chunk", 100, fixedChunks(100, 100), false},
		{"several chunks", 10 * transfer.MinChunkSize,
			fixedChunks(10*transfer.MinChunkSize, transfer.MinChunkSize), false},
		{"content-defined lengths", 30, []transfer.Chunk{
			{Index: 0, Offset: 0, Length: 7},
			{Index: 1, Offset: 7, Length: 19},
			{Index: 2, Offset: 26, Length: 4},
		}, false},
		{"short of the file", 101, fixedChunks(100, 100), true},
		{"past the end of the file", 99, fixedChunks(100, 100), true},
		{"a gap between two chunks", 20, []transfer.Chunk{
			{Index: 0, Offset: 0, Length: 10},
			{Index: 1, Offset: 11, Length: 10},
		}, true},
		{"an empty chunk", 10, []transfer.Chunk{
			{Index: 0, Offset: 0, Length: 0},
			{Index: 1, Offset: 0, Length: 10},
		}, true},
		{"an out-of-order index", 20, []transfer.Chunk{
			{Index: 1, Offset: 0, Length: 10},
			{Index: 0, Offset: 10, Length: 10},
		}, true},
		{"a chunk longer than a chunk may be", 1 << 30, []transfer.Chunk{
			{Index: 0, Offset: 0, Length: 1 << 30},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := transfer.ValidateChunks(tt.size, tt.chunks)
			if tt.wantErr != (err != nil) {
				t.Fatalf("ValidateChunks(%d, %v) = %v, wantErr %v", tt.size, tt.chunks, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, transfer.ErrInvalidChunks) {
				t.Fatalf("error %v does not name ErrInvalidChunks", err)
			}
		})
	}
}

func TestNewInbound_ChunkLayout(t *testing.T) {
	tr := newTransfer(t, transfer.MinChunkSize+100, transfer.MinChunkSize)

	if tr.State != transfer.StatePending {
		t.Fatalf("State = %q, want pending", tr.State)
	}
	if len(tr.Chunks) != 2 {
		t.Fatalf("len(Chunks) = %d, want 2", len(tr.Chunks))
	}
	if tr.Chunks[0].Offset != 0 || tr.Chunks[0].Length != transfer.MinChunkSize {
		t.Fatalf("chunk 0 = %+v, want full-length at offset 0", tr.Chunks[0])
	}
	if tr.Chunks[1].Offset != transfer.MinChunkSize || tr.Chunks[1].Length != 100 {
		t.Fatalf("chunk 1 = %+v, want 100 bytes at offset %d", tr.Chunks[1], transfer.MinChunkSize)
	}
}

func TestMarkVerified_OutOfRange(t *testing.T) {
	tr := newTransfer(t, 3*transfer.MinChunkSize, transfer.MinChunkSize)
	now := time.Unix(1700000002, 0).UTC()

	tests := []struct {
		name    string
		idx     int
		wantErr bool
	}{
		{"first", 0, false},
		{"last", 2, false},
		{"negative", -1, true},
		{"past end", 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tr.MarkVerified(tt.idx, hashOf(2), now)
			if tt.wantErr != (err != nil) {
				t.Fatalf("MarkVerified(%d) error = %v, wantErr %v", tt.idx, err, tt.wantErr)
			}
			if err == nil && !tr.Chunks[tt.idx].Verified {
				t.Fatalf("chunk %d not marked verified", tt.idx)
			}
		})
	}
}

func TestMissingChunks_AfterPartialVerify(t *testing.T) {
	tr := newTransfer(t, 3*transfer.MinChunkSize, transfer.MinChunkSize)
	now := time.Unix(1700000002, 0).UTC()

	if err := tr.MarkVerified(1, hashOf(2), now); err != nil {
		t.Fatalf("MarkVerified error: %v", err)
	}

	missing := tr.MissingChunks()
	if len(missing) != 2 || missing[0] != 0 || missing[1] != 2 {
		t.Fatalf("MissingChunks() = %v, want [0 2]", missing)
	}
	if got, want := tr.BytesDone(), int64(transfer.MinChunkSize); got != want {
		t.Fatalf("BytesDone() = %d, want %d", got, want)
	}
	if tr.UpdatedAt != now {
		t.Fatalf("UpdatedAt = %v, want %v", tr.UpdatedAt, now)
	}

	tr.ResetChunks()
	if len(tr.MissingChunks()) != 3 || tr.BytesDone() != 0 {
		t.Fatalf("after ResetChunks: missing=%v bytesDone=%d, want all missing", tr.MissingChunks(), tr.BytesDone())
	}
}

func TestComplete(t *testing.T) {
	now := time.Unix(1700000002, 0).UTC()

	t.Run("empty file is complete", func(t *testing.T) {
		tr := newTransfer(t, 0, transfer.MinChunkSize)
		if !tr.Complete() {
			t.Fatal("Complete() = false, want true for a zero-chunk transfer")
		}
	})

	t.Run("complete after all chunks verified", func(t *testing.T) {
		tr := newTransfer(t, 2*transfer.MinChunkSize, transfer.MinChunkSize)
		if tr.Complete() {
			t.Fatal("Complete() = true before verification")
		}
		for i := range tr.Chunks {
			if err := tr.MarkVerified(i, hashOf(byte(i)+1), now); err != nil {
				t.Fatalf("MarkVerified(%d) error: %v", i, err)
			}
		}
		if !tr.Complete() {
			t.Fatal("Complete() = false after verifying every chunk")
		}
		if got, want := tr.BytesDone(), int64(2*transfer.MinChunkSize); got != want {
			t.Fatalf("BytesDone() = %d, want %d", got, want)
		}
	})

	t.Run("chunk size below minimum falls back to default", func(t *testing.T) {
		tr := newTransfer(t, transfer.DefaultChunkSize, 10)
		if tr.ChunkSize != transfer.DefaultChunkSize {
			t.Fatalf("ChunkSize = %d, want %d", tr.ChunkSize, transfer.DefaultChunkSize)
		}
	})
}

// TestChunkIndex_Describes covers what geometry cannot answer: which content a
// stored cut was made from. Two versions of a path that are the same length
// produce chunk lists that both partition the file, so the hash the index was
// written with is the only thing that tells them apart.
func TestChunkIndex_Describes(t *testing.T) {
	const size = 4 * transfer.MinChunkSize
	fresh := transfer.ChunkIndex{
		Hash: hashOf(1), Chunks: fixedChunks(size, transfer.MinChunkSize),
	}

	if err := fresh.Describes(hashOf(1), size); err != nil {
		t.Fatalf("an index of this content is refused: %v", err)
	}
	// Same lengths, same offsets, other content: the case a length check
	// waves through.
	err := fresh.Describes(hashOf(2), size)
	if !errors.Is(err, transfer.ErrStaleIndex) {
		t.Fatalf("an index cut from other content = %v, want ErrStaleIndex", err)
	}
	// A cut of the right content that does not partition the file is still
	// unusable, and is reported as the geometry error it is.
	short := transfer.ChunkIndex{Hash: hashOf(1), Chunks: fixedChunks(size/2, transfer.MinChunkSize)}
	if err := short.Describes(hashOf(1), size); !errors.Is(err, transfer.ErrInvalidChunks) {
		t.Fatalf("an index that covers half the file = %v, want ErrInvalidChunks", err)
	}
	// A zero-byte file has no chunks, and an index with none describes it.
	var empty transfer.ChunkIndex
	if err := empty.Describes(manifest.Hash{}, 0); err != nil {
		t.Fatalf("the empty index of an empty file is refused: %v", err)
	}
}
