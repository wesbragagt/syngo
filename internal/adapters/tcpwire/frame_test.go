package tcpwire_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/wesbragagt/syngo/internal/adapters/tcpwire"
)

func TestFrame_RoundTrip(t *testing.T) {
	cases := []tcpwire.Frame{
		{Type: tcpwire.MsgPing, Header: []byte(`{"nonce":7}`)},
		{Type: tcpwire.MsgHello, Header: []byte(`{}`), Payload: nil},
		{Type: tcpwire.MsgChunkData, Header: []byte(`{"index":3}`), Payload: bytes.Repeat([]byte{0xAB}, 4096)},
	}
	for _, want := range cases {
		var buf bytes.Buffer
		if err := tcpwire.WriteFrame(&buf, want); err != nil {
			t.Fatalf("WriteFrame(%s) error: %v", want.Type, err)
		}
		got, err := tcpwire.ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame(%s) error: %v", want.Type, err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Header, want.Header) {
			t.Fatalf("frame = %s %q, want %s %q", got.Type, got.Header, want.Type, want.Header)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("payload of %d bytes, want %d", len(got.Payload), len(want.Payload))
		}
		if buf.Len() != 0 {
			t.Fatalf("%d bytes left after the frame", buf.Len())
		}
	}
}

func TestFrame_TooLargeRejected(t *testing.T) {
	var prefix bytes.Buffer
	_ = binary.Write(&prefix, binary.BigEndian, uint32(tcpwire.MaxFrame+1))
	if _, err := tcpwire.ReadFrame(&prefix); !errors.Is(err, tcpwire.ErrFrameTooLarge) {
		t.Fatalf("ReadFrame oversized = %v, want ErrFrameTooLarge", err)
	}
	// The body is never read, so the reader is not drained either.
	if prefix.Len() != 0 {
		t.Fatalf("%d bytes left, want the prefix consumed", prefix.Len())
	}

	oversized := tcpwire.Frame{Type: tcpwire.MsgChunkData, Payload: make([]byte, tcpwire.MaxFrame)}
	if err := tcpwire.WriteFrame(io.Discard, oversized); !errors.Is(err, tcpwire.ErrFrameTooLarge) {
		t.Fatalf("WriteFrame oversized = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrame_TruncatedReturnsUnexpectedEOF(t *testing.T) {
	var buf bytes.Buffer
	if err := tcpwire.WriteFrame(&buf, tcpwire.Frame{Type: tcpwire.MsgPing, Header: []byte(`{"nonce":1}`)}); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}
	encoded := buf.Bytes()

	if _, err := tcpwire.ReadFrame(bytes.NewReader(encoded[:len(encoded)-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame truncated body = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err := tcpwire.ReadFrame(bytes.NewReader(encoded[:2])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame truncated prefix = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err := tcpwire.ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame on a closed stream = %v, want io.EOF", err)
	}
}

func TestFrame_HeaderLongerThanBodyRejected(t *testing.T) {
	body := []byte{byte(tcpwire.MsgPing), 0, 0, 0, 40}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(body)))
	buf.Write(body)

	if _, err := tcpwire.ReadFrame(&buf); !errors.Is(err, tcpwire.ErrMalformedFrame) {
		t.Fatalf("ReadFrame malformed = %v, want ErrMalformedFrame", err)
	}
}
