package ports

import (
	"io"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Hasher computes content hashes.
type Hasher interface {
	// Sum reads r to EOF and returns the hash of its contents.
	Sum(r io.Reader) (manifest.Hash, error)
	// SumBytes returns the hash of b.
	SumBytes(b []byte) manifest.Hash
}
