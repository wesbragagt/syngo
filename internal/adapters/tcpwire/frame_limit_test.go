package tcpwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// TestReadFrameLimit_RefusesAFrameAboveTheLimit proves the pre-Hello limit is
// applied to the declared size, before the body is read: a peer that has not
// proved who it is must not be able to make this side allocate at will.
func TestReadFrameLimit_RefusesAFrameAboveTheLimit(t *testing.T) {
	var buf bytes.Buffer
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, MaxHelloFrame+1)
	buf.Write(header)
	// No body follows: a reader that respects the limit never asks for it.

	if _, err := readFrameLimit(&buf, MaxHelloFrame); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("readFrameLimit = %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes left unread, want the reader to have stopped at the prefix", buf.Len())
	}
}

func TestReadFrameLimit_AcceptsAFrameAtTheLimit(t *testing.T) {
	var buf bytes.Buffer
	payload := make([]byte, MaxHelloFrame-frameOverhead)
	if err := WriteFrame(&buf, Frame{Type: MsgHello, Header: payload}); err != nil {
		t.Fatalf("WriteFrame error: %v", err)
	}
	frame, err := readFrameLimit(&buf, MaxHelloFrame)
	if err != nil {
		t.Fatalf("readFrameLimit error: %v", err)
	}
	if len(frame.Header) != len(payload) {
		t.Fatalf("header of %d bytes, want %d", len(frame.Header), len(payload))
	}
}
