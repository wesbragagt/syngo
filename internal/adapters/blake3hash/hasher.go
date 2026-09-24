// Package blake3hash provides a ports.Hasher backed by BLAKE3.
package blake3hash

import (
	"fmt"
	"io"

	"lukechampine.com/blake3"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
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

// Stream returns an incremental BLAKE3 hasher, so one pass over a file yields
// both its whole-file hash and the hash of every chunk cut out of it.
func (Hasher) Stream() ports.Stream { return &stream{h: blake3.New(manifest.HashSize, nil)} }

// stream is an incremental BLAKE3 digest.
type stream struct{ h *blake3.Hasher }

// Write adds bytes to the digest. It never fails.
func (s *stream) Write(p []byte) (int, error) { return s.h.Write(p) }

// Sum returns the hash of everything written so far.
func (s *stream) Sum() manifest.Hash {
	var out manifest.Hash
	copy(out[:], s.h.Sum(nil))
	return out
}
