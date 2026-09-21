package fakes

import (
	"crypto/sha256"
	"io"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Hasher is a deterministic ports.Hasher backed by SHA-256. Domain code only
// needs the hash to be stable, not to be BLAKE3.
type Hasher struct{}

// NewHasher returns a Hasher.
func NewHasher() Hasher { return Hasher{} }

// Sum reads r to EOF and returns the hash of its contents.
func (Hasher) Sum(r io.Reader) (manifest.Hash, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return manifest.Hash{}, err
	}
	var out manifest.Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

// SumBytes returns the hash of b.
func (Hasher) SumBytes(b []byte) manifest.Hash {
	sum := sha256.Sum256(b)
	var out manifest.Hash
	copy(out[:], sum[:])
	return out
}
