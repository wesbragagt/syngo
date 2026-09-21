// Package transfer models an in-progress inbound file download and its chunks.
package transfer

import (
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Chunk sizing limits.
const (
	DefaultChunkSize = 1 << 20
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

// NewInbound builds a pending transfer for entry e, with one unverified chunk
// per chunk of the file.
func NewInbound(id ID, f folder.ID, e manifest.Entry, peer device.ID, chunkSize int, tempPath string, now time.Time) Transfer {
	if chunkSize < MinChunkSize {
		chunkSize = DefaultChunkSize
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
		Chunks:    newChunks(e.Size, chunkSize),
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

// ResetChunks clears all verification state, for when the partial file is lost.
func (t *Transfer) ResetChunks() {
	for i := range t.Chunks {
		t.Chunks[i].Hash = manifest.Hash{}
		t.Chunks[i].Verified = false
	}
}
