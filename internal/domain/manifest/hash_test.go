package manifest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

func TestParseHash_RoundTrip(t *testing.T) {
	hex := strings.Repeat("ab", manifest.HashSize)

	h, err := manifest.ParseHash(hex)
	if err != nil {
		t.Fatalf("ParseHash error: %v", err)
	}
	if h.IsZero() {
		t.Fatal("parsed hash is zero")
	}
	if got := h.String(); got != hex {
		t.Fatalf("String() = %q, want %q", got, hex)
	}
}

func TestParseHash_ZeroAndInvalid(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty is zero", "", false},
		{"short", "abcd", true},
		{"long", strings.Repeat("a", 65), true},
		{"not hex", strings.Repeat("z", 64), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := manifest.ParseHash(tt.input)
			if tt.wantErr {
				if !errors.Is(err, manifest.ErrInvalidHash) {
					t.Fatalf("ParseHash(%q) error = %v, want ErrInvalidHash", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHash(%q) error: %v", tt.input, err)
			}
			if !h.IsZero() || h.String() != "" {
				t.Fatalf("zero hash round trip failed: %q", h.String())
			}
		})
	}
}
