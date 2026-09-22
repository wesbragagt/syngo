package fakes

import (
	"crypto/sha256"
	"hash"
	"io"

	"github.com/wesbragagt/syncthis/internal/app/ports"
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

// Stream returns an incremental SHA-256 digest.
func (Hasher) Stream() ports.Stream { return &stream{h: sha256.New()} }

// stream is an incremental SHA-256 digest.
type stream struct{ h hash.Hash }

// Write adds bytes to the digest. It never fails.
func (s *stream) Write(p []byte) (int, error) { return s.h.Write(p) }

// Sum returns the hash of everything written so far.
func (s *stream) Sum() manifest.Hash {
	var out manifest.Hash
	copy(out[:], s.h.Sum(nil))
	return out
}

// SumBytes returns the hash of b.
func (Hasher) SumBytes(b []byte) manifest.Hash {
	sum := sha256.Sum256(b)
	var out manifest.Hash
	copy(out[:], sum[:])
	return out
}
