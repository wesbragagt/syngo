// Package tcpwire speaks the syncthis peer protocol: length-prefixed frames
// over TLS 1.3 on TCP, with device identities derived from the certificates.
// docs/PROTOCOL.md describes the wire format.
package tcpwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MsgType identifies the kind of a frame.
type MsgType uint8

// Message types. The numbers are part of the wire protocol.
const (
	MsgHello           MsgType = 0x01
	MsgRejected        MsgType = 0x02
	MsgPing            MsgType = 0x03
	MsgPong            MsgType = 0x04
	MsgManifestRequest MsgType = 0x10
	MsgManifestReply   MsgType = 0x11
	MsgManifestChanged MsgType = 0x12
	MsgFoldersChanged  MsgType = 0x13
	MsgChunkRequest    MsgType = 0x20
	MsgChunkData       MsgType = 0x21
	MsgChunkError      MsgType = 0x22
	MsgError           MsgType = 0xF0
)

// String names the message type for logs and errors.
func (t MsgType) String() string {
	switch t {
	case MsgHello:
		return "hello"
	case MsgRejected:
		return "rejected"
	case MsgPing:
		return "ping"
	case MsgPong:
		return "pong"
	case MsgManifestRequest:
		return "manifest_request"
	case MsgManifestReply:
		return "manifest_reply"
	case MsgManifestChanged:
		return "manifest_changed"
	case MsgFoldersChanged:
		return "folders_changed"
	case MsgChunkRequest:
		return "chunk_request"
	case MsgChunkData:
		return "chunk_data"
	case MsgChunkError:
		return "chunk_error"
	case MsgError:
		return "error"
	default:
		return fmt.Sprintf("0x%02x", uint8(t))
	}
}

// MaxFrame is the largest frame body accepted or produced, excluding the
// length prefix itself.
const MaxFrame = 64 << 20

// MaxHelloFrame is the largest frame accepted before the Hello exchange has
// finished. Anyone who can complete a TLS handshake can send frames, so what a
// peer may make this device allocate before it has proved who it is stays
// small; a Hello is a few hundred bytes.
const MaxHelloFrame = 64 << 10

// frameOverhead is the type byte plus the header length field.
const frameOverhead = 1 + 4

// ErrFrameTooLarge reports a frame larger than MaxFrame. The connection is not
// usable afterwards, because the reader cannot find the next frame boundary.
var ErrFrameTooLarge = errors.New("frame exceeds the maximum size")

// ErrMalformedFrame reports a frame whose declared sizes do not add up.
var ErrMalformedFrame = errors.New("malformed frame")

// Frame is one protocol message: a type, a JSON header and an optional
// payload. Only chunk data carries a payload.
type Frame struct {
	Type    MsgType
	Header  []byte
	Payload []byte
}

// WriteFrame writes f in one call, so concurrent writers cannot interleave
// parts of two frames when the caller serializes at this granularity.
func WriteFrame(w io.Writer, f Frame) error {
	body := frameOverhead + len(f.Header) + len(f.Payload)
	if body > MaxFrame {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, body)
	}

	buf := make([]byte, 4+body)
	binary.BigEndian.PutUint32(buf[0:4], uint32(body))
	buf[4] = byte(f.Type)
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(f.Header)))
	copy(buf[9:], f.Header)
	copy(buf[9+len(f.Header):], f.Payload)

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write %s frame: %w", f.Type, err)
	}
	return nil
}

// ReadFrame reads one frame. A frame larger than MaxFrame fails with
// ErrFrameTooLarge without reading its body; a truncated frame fails with
// io.ErrUnexpectedEOF. A clean close before any byte yields io.EOF.
func ReadFrame(r io.Reader) (Frame, error) { return readFrameLimit(r, MaxFrame) }

// readFrameLimit reads one frame no larger than limit.
func readFrameLimit(r io.Reader, limit uint32) (Frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return Frame{}, err
	}
	body := binary.BigEndian.Uint32(prefix[:])
	switch {
	case body > limit:
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, body)
	case body < frameOverhead:
		return Frame{}, fmt.Errorf("%w: body of %d bytes", ErrMalformedFrame, body)
	}

	buf := make([]byte, body)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Frame{}, err
	}
	headerLen := binary.BigEndian.Uint32(buf[1:5])
	if uint64(headerLen) > uint64(body-frameOverhead) {
		return Frame{}, fmt.Errorf("%w: header of %d bytes in a body of %d", ErrMalformedFrame, headerLen, body)
	}

	return Frame{
		Type:    MsgType(buf[0]),
		Header:  buf[frameOverhead : frameOverhead+headerLen],
		Payload: buf[frameOverhead+headerLen:],
	}, nil
}
