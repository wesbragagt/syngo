// Package device defines device identity values: the 52-character device ID
// derived from a public key, and the Device record describing a known peer.
package device

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IDLength is the number of characters in the canonical (undashed) device ID.
const IDLength = 52

// groupSize is the number of characters between dashes in the display form.
const groupSize = 13

// ErrInvalidID is returned by ParseID when the input is not a device ID.
var ErrInvalidID = errors.New("invalid device id")

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ID is a device identifier: base32 of the SHA-256 of the device's SubjectPublicKeyInfo.
type ID string

// IDFromPublicKey derives a device ID from a DER-encoded SubjectPublicKeyInfo,
// such as x509.Certificate.RawSubjectPublicKeyInfo.
func IDFromPublicKey(spkiDER []byte) ID {
	sum := sha256.Sum256(spkiDER)
	return ID(idEncoding.EncodeToString(sum[:]))
}

// ParseID accepts either the canonical 52-character form or the display form
// with dashes every 13 characters, and returns the canonical form.
func ParseID(s string) (ID, error) {
	canonical := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
	if len(canonical) != IDLength {
		return "", fmt.Errorf("%w: got %d characters, want %d", ErrInvalidID, len(canonical), IDLength)
	}
	if _, err := idEncoding.DecodeString(canonical); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidID, err)
	}
	return ID(canonical), nil
}

// Short returns the first 7 characters of the ID, for logs and conflict names.
func (id ID) Short() string {
	if len(id) < 7 {
		return string(id)
	}
	return string(id[:7])
}

// Display returns the ID in dashed groups of 13 characters.
func (id ID) Display() string {
	s := string(id)
	var b strings.Builder
	for i := 0; i < len(s); i += groupSize {
		if i > 0 {
			b.WriteByte('-')
		}
		end := i + groupSize
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
	}
	return b.String()
}

// String returns the canonical form of the ID.
func (id ID) String() string { return string(id) }

// Device is a known remote device.
type Device struct {
	ID         ID
	Name       string
	Address    string
	Verified   bool
	AddedAt    time.Time
	LastSeenAt time.Time
}
