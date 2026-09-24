package ports

import (
	"io"

	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// Hasher computes content hashes.
type Hasher interface {
	// Sum reads r to EOF and returns the hash of its contents.
	Sum(r io.Reader) (manifest.Hash, error)
	// SumBytes returns the hash of b.
	SumBytes(b []byte) manifest.Hash
	// Stream returns a hasher fed in pieces. It is what lets one pass over
	// a file produce both the whole-file hash and the hash of every chunk
	// the chunker cuts out of it.
	Stream() Stream
}

// Stream is an incremental hash: everything written to it, hashed once Sum is
// called. A Stream is used by one goroutine at a time.
type Stream interface {
	io.Writer
	// Sum returns the hash of everything written so far.
	Sum() manifest.Hash
}
