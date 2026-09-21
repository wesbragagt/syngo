package device_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/device"
)

func TestIDFromPublicKey_Stable(t *testing.T) {
	spki := []byte("subject-public-key-info")
	first := device.IDFromPublicKey(spki)
	second := device.IDFromPublicKey(spki)

	if first != second {
		t.Fatalf("IDFromPublicKey not stable: %q != %q", first, second)
	}
	if len(first) != device.IDLength {
		t.Fatalf("len(id) = %d, want %d", len(first), device.IDLength)
	}
	if got := device.IDFromPublicKey([]byte("other")); got == first {
		t.Fatal("different keys produced the same id")
	}
	if strings.ToUpper(string(first)) != string(first) {
		t.Fatalf("id is not uppercase: %q", first)
	}
}

func TestParseID_AcceptsDisplayForm(t *testing.T) {
	id := device.IDFromPublicKey([]byte("display-form"))

	parsed, err := device.ParseID(id.Display())
	if err != nil {
		t.Fatalf("ParseID(%q) error: %v", id.Display(), err)
	}
	if parsed != id {
		t.Fatalf("ParseID(display) = %q, want %q", parsed, id)
	}
	if got := strings.Count(id.Display(), "-"); got != 3 {
		t.Fatalf("display form has %d dashes, want 4", got)
	}
	if got := id.Short(); got != string(id)[:7] {
		t.Fatalf("Short() = %q, want %q", got, string(id)[:7])
	}
}

func TestParseID_RejectsBadLength(t *testing.T) {
	valid := string(device.IDFromPublicKey([]byte("bad-length")))

	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"too short", valid[:10]},
		{"too long", valid + "AAAA"},
		{"not base32", strings.Repeat("1", device.IDLength)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := device.ParseID(tt.input); !errors.Is(err, device.ErrInvalidID) {
				t.Fatalf("ParseID(%q) error = %v, want ErrInvalidID", tt.input, err)
			}
		})
	}
}
