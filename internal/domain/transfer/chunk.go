package transfer

import "github.com/wesbragagt/syncthis/internal/domain/manifest"

// Chunk is one fixed-size piece of a transferred file. The final chunk may be
// shorter than the transfer's chunk size.
type Chunk struct {
	Index    int
	Offset   int64
	Length   int
	Hash     manifest.Hash
	Verified bool
}

// MaxChunks is the largest number of chunks one transfer may be split into.
// The chunk list is held in memory and journalled row by row, so a peer that
// announces an absurd size must not be able to decide how much memory and how
// many database rows this device spends. At the default chunk size it allows a
// file of four tebibytes.
const MaxChunks = 4 << 20

// ChunkCount returns how many chunks a file of the given size is split into. A
// zero-byte file has no chunks.
func ChunkCount(size int64, chunkSize int) int {
	if size <= 0 || chunkSize <= 0 {
		return 0
	}
	return int((size + int64(chunkSize) - 1) / int64(chunkSize))
}

// newChunks builds the unverified chunk list for a file of the given size.
func newChunks(size int64, chunkSize int) []Chunk {
	n := ChunkCount(size, chunkSize)
	chunks := make([]Chunk, n)
	for i := range chunks {
		offset := int64(i) * int64(chunkSize)
		length := int64(chunkSize)
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		chunks[i] = Chunk{Index: i, Offset: offset, Length: int(length)}
	}
	return chunks
}
