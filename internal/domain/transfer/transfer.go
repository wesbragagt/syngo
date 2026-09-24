// Package transfer models an in-progress inbound file download and its chunks.
package transfer

import (
	"fmt"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// Chunk sizing. Since content-defined chunking the configured size is the
// average a folder aims for, not the length of every chunk: the chunker cuts
// between a quarter and four times it, wherever the content says.
//
// The default is 64 KiB because that is where the benchmark claim holds: an
// insertion costs its own bytes plus the chunks it re-cuts, and re-cutting
// takes one to four chunks depending on where the boundaries fall, so a
// 1 MiB insertion into a large file stays under the 2 MiB budget only once
// four average chunks fit in the megabyte of slack. Measured at eight
// averages and six insertion positions in docs/BENCH.md: 1 MiB is over at
// every position, 256 KiB at two of six, 64 KiB at none, with 31% to spare.
// What it costs is measured there too, and paid where it is cheapest: about
// 0.5% of the content in chunk index rows, and a chunking throughput ceiling
// of about 240 MiB/s, which is above any network Syngo synchronizes over.
const (
	DefaultChunkSize = 64 << 10
	MinChunkSize     = 4096
)

// ID identifies a transfer. It is 32 hex characters.
type ID string

// String returns the transfer ID as a string.
func (id ID) String() string { return string(id) }

// State is the lifecycle state of a transfer.
type State string

// Transfer states.
const (
	StatePending   State = "pending"
	StateActive    State = "active"
	StatePaused    State = "paused"
	StateVerifying State = "verifying"
	StateDone      State = "done"
	StateFailed    State = "failed"
)

// Transfer is an inbound file download from a peer. The sender keeps no state.
type Transfer struct {
	ID        ID
	FolderID  folder.ID
	Path      string
	PeerID    device.ID
	Hash      manifest.Hash
	Size      int64
	MTimeNS   int64
	Mode      uint32
	ChunkSize int
	TempPath  string
	State     State
	Chunks    []Chunk
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewInbound builds a pending transfer for entry e over the chunk list the
// sender announced. Every chunk starts unverified, keeping the hash the sender
// gave it: that hash is what the receiver looks up in what it already holds
// before asking for the bytes, and what it checks the bytes against either way.
//
// chunkSize is the average this device asked for, recorded for the journal
// only; the boundaries are the sender's.
func NewInbound(id ID, f folder.ID, e manifest.Entry, peer device.ID, chunks []Chunk,
	chunkSize int, tempPath string, now time.Time) Transfer {
	if chunkSize < MinChunkSize {
		chunkSize = DefaultChunkSize
	}
	owned := make([]Chunk, len(chunks))
	for i, c := range chunks {
		owned[i] = Chunk{Index: i, Offset: c.Offset, Length: c.Length, Hash: c.Hash}
	}
	return Transfer{
		ID:        id,
		FolderID:  f,
		Path:      e.Path,
		PeerID:    peer,
		Hash:      e.Hash,
		Size:      e.Size,
		MTimeNS:   e.ModTime.UnixNano(),
		Mode:      e.Mode,
		ChunkSize: chunkSize,
		TempPath:  tempPath,
		State:     StatePending,
		Chunks:    owned,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// MissingChunks returns the indexes of chunks that are not yet verified, in
// ascending order.
func (t Transfer) MissingChunks() []int {
	var out []int
	for _, c := range t.Chunks {
		if !c.Verified {
			out = append(out, c.Index)
		}
	}
	return out
}

// BytesDone returns the number of bytes covered by verified chunks.
func (t Transfer) BytesDone() int64 {
	var total int64
	for _, c := range t.Chunks {
		if c.Verified {
			total += int64(c.Length)
		}
	}
	return total
}

// Complete reports whether every chunk is verified.
func (t Transfer) Complete() bool {
	for _, c := range t.Chunks {
		if !c.Verified {
			return false
		}
	}
	return true
}

// MarkVerified records the hash of a received chunk and marks it verified.
func (t *Transfer) MarkVerified(idx int, h manifest.Hash, now time.Time) error {
	if idx < 0 || idx >= len(t.Chunks) {
		return fmt.Errorf("chunk index %d out of range [0,%d)", idx, len(t.Chunks))
	}
	t.Chunks[idx].Hash = h
	t.Chunks[idx].Verified = true
	t.UpdatedAt = now
	return nil
}

// ResetChunks clears the verification state, for when the partial file is
// lost. The hashes stay: they are what the sender announced, not what this
// device measured, so they are still the list of what has to be found or
// fetched.
func (t *Transfer) ResetChunks() {
	for i := range t.Chunks {
		t.Chunks[i].Verified = false
	}
}
