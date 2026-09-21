// Package blake3hash provides a ports.Hasher backed by BLAKE3.
package blake3hash

import (
	"fmt"
	"io"

	"lukechampine.com/blake3"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

var _ ports.Hasher = Hasher{}

// Hasher computes 256-bit BLAKE3 digests.
type Hasher struct{}

// New returns a BLAKE3 hasher.
func New() Hasher { return Hasher{} }

// Sum reads r to EOF and returns the BLAKE3 hash of its contents.
func (Hasher) Sum(r io.Reader) (manifest.Hash, error) {
	h := blake3.New(manifest.HashSize, nil)
	if _, err := io.Copy(h, r); err != nil {
		return manifest.Hash{}, fmt.Errorf("blake3 sum: %w", err)
	}
	var out manifest.Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

// SumBytes returns the BLAKE3 hash of b.
func (Hasher) SumBytes(b []byte) manifest.Hash {
	return manifest.Hash(blake3.Sum256(b))
}
