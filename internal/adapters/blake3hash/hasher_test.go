package blake3hash_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/adapters/blake3hash"
)

func TestSum_KnownVector(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"},
		{"abc", "abc", "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"},
	}
	h := blake3hash.New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.SumBytes([]byte(tt.input)).String(); got != tt.want {
				t.Fatalf("SumBytes(%q) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestSumReader_EqualsSumBytes(t *testing.T) {
	h := blake3hash.New()
	data := bytes.Repeat([]byte("syncthis"), 5000)

	fromReader, err := h.Sum(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("Sum error: %v", err)
	}
	if fromBytes := h.SumBytes(data); fromReader != fromBytes {
		t.Fatalf("Sum = %s, SumBytes = %s", fromReader, fromBytes)
	}
}
