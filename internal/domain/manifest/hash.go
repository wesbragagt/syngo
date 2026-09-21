// Package manifest describes the contents of a folder as a set of entries, and
// computes the changes between two snapshots of it.
package manifest

import (
	"encoding/hex"
	"errors"
	"fmt"
)

// HashSize is the length in bytes of a content hash.
const HashSize = 32

// ErrInvalidHash is returned by ParseHash when the input is not a hex hash.
var ErrInvalidHash = errors.New("invalid hash")

// Hash is a BLAKE3-256 content hash.
type Hash [HashSize]byte

// String returns the hash as 64 lowercase hex characters, or "" if zero.
func (h Hash) String() string {
	if h.IsZero() {
		return ""
	}
	return hex.EncodeToString(h[:])
}

// IsZero reports whether the hash is unset.
func (h Hash) IsZero() bool {
	return h == Hash{}
}

// ParseHash decodes 64 hex characters into a Hash. The empty string parses to
// the zero hash, which is how directories, symlinks and tombstones are stored.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if s == "" {
		return h, nil
	}
	if len(s) != hex.EncodedLen(HashSize) {
		return h, fmt.Errorf("%w: got %d characters, want %d", ErrInvalidHash, len(s), hex.EncodedLen(HashSize))
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return Hash{}, fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
	return h, nil
}
