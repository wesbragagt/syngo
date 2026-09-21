package transfer_test

import (
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

func hashOf(seed byte) manifest.Hash {
	var h manifest.Hash
	for i := range h {
		h[i] = seed
	}
	return h
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
	return transfer.NewInbound("abc", "docs", entry, "PEER", chunkSize, "/tmp/abc.part", now)
}

func TestChunkCount(t *testing.T) {
	tests := []struct {
		name      string
		size      int64
		chunkSize int
		want      int
	}{
		{"empty file", 0, transfer.DefaultChunkSize, 0},
		{"one byte", 1, transfer.DefaultChunkSize, 1},
		{"exactly one chunk", transfer.DefaultChunkSize, transfer.DefaultChunkSize, 1},
		{"one byte over", transfer.DefaultChunkSize + 1, transfer.DefaultChunkSize, 2},
		{"many chunks", 10 * transfer.MinChunkSize, transfer.MinChunkSize, 10},
		{"negative size", -5, transfer.DefaultChunkSize, 0},
		{"zero chunk size", 100, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transfer.ChunkCount(tt.size, tt.chunkSize); got != tt.want {
				t.Fatalf("ChunkCount(%d, %d) = %d, want %d", tt.size, tt.chunkSize, got, tt.want)
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
