package transfer

import (
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/domain/chunker"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Chunk is one content-defined piece of a transferred file. Offset and Length
// are where the sender cut it, not a position derived from a fixed size: two
// devices agree on them because they run the same chunker, and the sender puts
// them on the wire so the receiver can look each chunk up before asking for it.
type Chunk struct {
	Index    int
	Offset   int64
	Length   int
	Hash     manifest.Hash
	Verified bool
}

// MaxChunks is the largest number of chunks one transfer may be split into.
// The whole list travels in one frame, is held in memory and is journalled row
// by row, so a peer that announces an absurd file must not be able to decide
// how much memory, how many database rows and how large a frame this device
// spends. One chunk costs 36 bytes on the wire, so a million of them stay
// inside the 64 MiB frame limit, and at the minimum chunk size of the default
// configuration a million chunks is a file of 256 GiB.
const MaxChunks = 1 << 20

// ErrInvalidChunks reports a chunk list that does not describe the file it
// came with. A peer supplies the list, so it is checked before anything is
// built from it.
var ErrInvalidChunks = errors.New("invalid chunk list")

// ValidateChunks reports whether chunks partition a file of the given size:
// one chunk after another from offset zero, none empty, none longer than a
// chunk may be, and together exactly as long as the file. A zero-byte file has
// no chunks.
func ValidateChunks(size int64, chunks []Chunk) error {
	if len(chunks) > MaxChunks {
		return fmt.Errorf("%w: %d chunks, the limit is %d", ErrInvalidChunks, len(chunks), MaxChunks)
	}
	var offset int64
	for i, c := range chunks {
		switch {
		case c.Index != i:
			return fmt.Errorf("%w: chunk %d carries index %d", ErrInvalidChunks, i, c.Index)
		case c.Offset != offset:
			return fmt.Errorf("%w: chunk %d starts at %d, want %d", ErrInvalidChunks, i, c.Offset, offset)
		case c.Length <= 0:
			return fmt.Errorf("%w: chunk %d is %d bytes", ErrInvalidChunks, i, c.Length)
		case c.Length > chunker.MaxSize:
			return fmt.Errorf("%w: chunk %d is %d bytes, the limit is %d",
				ErrInvalidChunks, i, c.Length, chunker.MaxSize)
		}
		offset += int64(c.Length)
	}
	if offset != size {
		return fmt.Errorf("%w: chunks cover %d bytes, the file is %d", ErrInvalidChunks, offset, size)
	}
	return nil
}

// FixedChunks cuts a file of the given size every chunkSize bytes, the way
// chunking worked before it became content-defined.
//
// It is how a transfer journalled by an older version is read back, so a
// download that was in flight across the upgrade resumes on the boundaries it
// was started with instead of beginning again. Nothing new is cut this way.
func FixedChunks(size int64, chunkSize int) []Chunk {
	if size <= 0 || chunkSize <= 0 {
		return nil
	}
	var out []Chunk
	for offset := int64(0); offset < size; offset += int64(chunkSize) {
		length := int64(chunkSize)
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		out = append(out, Chunk{Index: len(out), Offset: offset, Length: int(length)})
	}
	return out
}

// ChunkIndex is a stored chunk list together with the whole-file hash of the
// content it was cut from. The hash is what makes the list self-describing: a
// list of offsets and lengths says nothing about which bytes it was cut from,
// so an index left behind by an earlier version of a file is indistinguishable
// from a fresh one whenever the two versions happen to be the same length.
type ChunkIndex struct {
	Hash   manifest.Hash
	Chunks []Chunk
}

// ErrStaleIndex reports a chunk index that was cut from content other than the
// one it is being read for.
var ErrStaleIndex = errors.New("stale chunk index")

// Describes reports whether the index was cut from exactly this content: the
// hash it was stored with is the file's, and its chunks partition a file of
// that size. A zero-byte file is described by an empty index.
func (ix ChunkIndex) Describes(hash manifest.Hash, size int64) error {
	if ix.Hash != hash {
		return fmt.Errorf("%w: cut from %s, the file is %s", ErrStaleIndex, ix.Hash.String(), hash.String())
	}
	return ValidateChunks(size, ix.Chunks)
}
