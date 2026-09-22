package tcpwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// frameBytes builds the wire form of a frame without going through
// WriteFrame, so a seed can declare sizes that WriteFrame would never write.
func frameBytes(body uint32, typ byte, headerLen uint32, rest []byte) []byte {
	out := make([]byte, 9, 9+len(rest))
	binary.BigEndian.PutUint32(out[0:4], body)
	out[4] = typ
	binary.BigEndian.PutUint32(out[5:9], headerLen)
	return append(out, rest...)
}

// fuzzFrameSeeds are the raw frames both fuzz targets start from: valid ones
// and the malformed shapes docs/PROTOCOL.md says are refused.
func fuzzFrameSeeds() [][]byte {
	hello := []byte(`{"protocol_version":3,"device_id":"` +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		`","device_name":"laptop","folders":[{"id":"photos","mode":"sendreceive"}]}`)
	ping := []byte(`{"nonce":123}`)
	chunk := []byte(`{"folder_id":"photos","path":"a.txt","index":0,"offset":0,"length":4,"chunk_hash":""}`)

	return [][]byte{
		// Valid shapes, one per direction of the handshake.
		frameBytes(uint32(frameOverhead+len(hello)), byte(MsgHello), uint32(len(hello)), hello),
		frameBytes(uint32(frameOverhead+len(ping)), byte(MsgPing), uint32(len(ping)), ping),
		frameBytes(uint32(frameOverhead+len(chunk)+4), byte(MsgChunkData), uint32(len(chunk)), append(append([]byte{}, chunk...), "data"...)),
		frameBytes(frameOverhead, byte(MsgPong), 0, nil),
		// Known-bad shapes.
		nil,
		{0x00},
		{0x00, 0x00, 0x00},
		{0xFF, 0xFF, 0xFF, 0xFF}, // a huge length and no body
		{0x00, 0x00, 0x00, 0x05, 0x01, 0x00, 0x00, 0x00, 0x00},
		frameBytes(0, 0, 0, nil),                         // body below the overhead
		frameBytes(4, 0, 0, nil),                         // body one byte short
		frameBytes(frameOverhead, byte(MsgPing), 1, nil), // header longer than the body
		frameBytes(frameOverhead+2, byte(MsgPing), 0xFFFFFFFF, []byte("ab")),
		frameBytes(0xFFFFFFFF, byte(MsgPing), 0, nil), // an attacker-chosen huge length
		frameBytes(MaxFrame+1, byte(MsgPing), 0, nil),
		frameBytes(MaxHelloFrame+1, byte(MsgHello), 0, nil),
		frameBytes(uint32(frameOverhead+len(ping)), byte(MsgPing), uint32(len(ping)), ping[:3]), // truncated
	}
}

// checkFrame asserts what a decoded frame must satisfy under limit: its parts
// fit the limit, and re-encoding it reproduces exactly the bytes it was read
// from.
func checkFrame(t *testing.T, data []byte, f Frame, limit uint32) {
	t.Helper()
	body := frameOverhead + len(f.Header) + len(f.Payload)
	if uint64(body) > uint64(limit) {
		t.Fatalf("decoded a frame of %d bytes, above the limit of %d", body, limit)
	}
	declared := binary.BigEndian.Uint32(data[0:4])
	if uint32(body) != declared {
		t.Fatalf("decoded %d body bytes from a frame declaring %d", body, declared)
	}

	var round bytes.Buffer
	if err := WriteFrame(&round, f); err != nil {
		t.Fatalf("re-encoding a decoded frame failed: %v", err)
	}
	if !bytes.Equal(round.Bytes(), data[:4+body]) {
		t.Fatalf("re-encoding a decoded frame changed it:\n got %x\nwant %x", round.Bytes(), data[:4+body])
	}
}

// checkFrameError asserts that a rejection names a reason the protocol
// defines, so no input reaches a caller as an unclassified failure.
func checkFrameError(t *testing.T, data []byte, err error, limit uint32) {
	t.Helper()
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, ErrFrameTooLarge), errors.Is(err, ErrMalformedFrame):
	default:
		t.Fatalf("ReadFrame rejected %x with an unexpected error: %v", data, err)
	}
	// A declared length above the limit is refused as too large, before
	// anything is allocated from it.
	if len(data) >= 4 && binary.BigEndian.Uint32(data[0:4]) > limit && !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("a frame declaring %d bytes was refused with %v, want ErrFrameTooLarge",
			binary.BigEndian.Uint32(data[0:4]), err)
	}
}

// FuzzReadFrame decodes arbitrary bytes as a frame. An accepted frame
// re-encodes to the bytes it came from; a refused one names a protocol error.
func FuzzReadFrame(f *testing.F) {
	for _, seed := range fuzzFrameSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			checkFrameError(t, data, err, MaxFrame)
			return
		}
		checkFrame(t, data, frame, MaxFrame)
	})
}

// FuzzReadHelloFrame decodes arbitrary bytes under the pre-handshake limit,
// which is what an unauthenticated peer can make this device allocate.
func FuzzReadHelloFrame(f *testing.F) {
	for _, seed := range fuzzFrameSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := readFrameLimit(bytes.NewReader(data), MaxHelloFrame)
		if err != nil {
			checkFrameError(t, data, err, MaxHelloFrame)
			return
		}
		checkFrame(t, data, frame, MaxHelloFrame)
	})
}

// FuzzWriteFrame writes arbitrary frames and reads them back. The invariant is
// the round trip: what WriteFrame produces, ReadFrame returns unchanged.
func FuzzWriteFrame(f *testing.F) {
	f.Add(uint8(MsgHello), []byte(`{"protocol_version":3}`), []byte(nil))
	f.Add(uint8(MsgChunkData), []byte(`{"length":4}`), []byte("data"))
	f.Add(uint8(MsgPing), []byte{}, []byte{})
	f.Add(uint8(0xAB), []byte{0x00, 0xFF}, []byte{0x00})

	f.Fuzz(func(t *testing.T, typ uint8, header, payload []byte) {
		out := Frame{Type: MsgType(typ), Header: header, Payload: payload}
		var buf bytes.Buffer
		if err := WriteFrame(&buf, out); err != nil {
			if !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("WriteFrame failed with an unexpected error: %v", err)
			}
			return
		}

		got, err := ReadFrame(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("reading back a written frame failed: %v", err)
		}
		if got.Type != out.Type {
			t.Fatalf("round trip changed the type: got %s, want %s", got.Type, out.Type)
		}
		if !bytes.Equal(got.Header, header) {
			t.Fatalf("round trip changed the header: got %x, want %x", got.Header, header)
		}
		if !bytes.Equal(got.Payload, payload) {
			t.Fatalf("round trip changed the payload: got %x, want %x", got.Payload, payload)
		}
		// The type has a name for every byte, so a log line never
		// depends on a type this build knows.
		if got.Type.String() == "" {
			t.Fatalf("message type 0x%02x has no name", typ)
		}
	})
}
