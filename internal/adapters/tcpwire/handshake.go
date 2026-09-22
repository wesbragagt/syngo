package tcpwire

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// Default timings of a connection. Tests shorten them; see export_test.go.
const (
	// DefaultPingInterval is how long a connection may stay idle before the
	// client sends a Ping.
	DefaultPingInterval = 30 * time.Second
	// DefaultIdleTimeout is how long either side waits for a frame before
	// it gives up on the connection.
	DefaultIdleTimeout = 90 * time.Second
	// DefaultDialTimeout bounds the TCP connect, the TLS handshake and the
	// Hello exchange together.
	DefaultDialTimeout = 10 * time.Second
)

// exchangeHello writes our Hello and reads the peer's. peerID is the identity
// derived from the peer's certificate; a Hello claiming another device is
// rejected, which is what binds the announced identity to the key. A Rejected
// frame from the peer is returned as a *RejectedError.
func exchangeHello(conn net.Conn, local ports.HelloInfo, peerID device.ID) (ports.HelloInfo, error) {
	frame, err := frameOf(MsgHello, toWireHello(local), nil)
	if err != nil {
		return ports.HelloInfo{}, err
	}
	if err := WriteFrame(conn, frame); err != nil {
		return ports.HelloInfo{}, err
	}

	// The peer has not proved who it is yet, so what it may make this side
	// allocate is bounded well below the frame size of a live connection.
	received, err := readFrameLimit(conn, MaxHelloFrame)
	if err != nil {
		return ports.HelloInfo{}, fmt.Errorf("read hello: %w", err)
	}
	switch received.Type {
	case MsgHello:
	case MsgRejected:
		var h rejectedHeader
		if err := decodeHeader(received, &h); err != nil {
			return ports.HelloInfo{}, err
		}
		return ports.HelloInfo{}, &ports.RejectedError{Reason: h.Reason}
	default:
		return ports.HelloInfo{}, fmt.Errorf("expected hello, got %s", received.Type)
	}

	var header helloHeader
	if err := decodeHeader(received, &header); err != nil {
		return ports.HelloInfo{}, err
	}
	if header.ProtocolVersion != ProtocolVersion {
		_ = writeRejected(conn, ReasonProtocolVersion)
		// The refusal is this side's, and it is the same outcome as the
		// peer refusing us for the same reason, so it carries the same
		// protocol reason: callers report rejected:protocol_version
		// either way instead of a bare "handshake".
		return ports.HelloInfo{}, &ports.RejectedError{
			Reason: ReasonProtocolVersion,
			Detail: fmt.Sprintf("peer speaks protocol version %d, want %d",
				header.ProtocolVersion, ProtocolVersion),
		}
	}
	hello, err := fromWireHello(header)
	if err != nil {
		_ = writeRejected(conn, ReasonIDMismatch)
		return ports.HelloInfo{}, err
	}
	if hello.DeviceID != peerID {
		_ = writeRejected(conn, ReasonIDMismatch)
		return ports.HelloInfo{}, fmt.Errorf("peer announced %s but its certificate is %s",
			hello.DeviceID.Short(), peerID.Short())
	}
	return hello, nil
}

// writeRejected tells the peer why the connection is refused.
func writeRejected(w io.Writer, reason string) error {
	frame, err := frameOf(MsgRejected, rejectedHeader{Reason: reason}, nil)
	if err != nil {
		return err
	}
	return WriteFrame(w, frame)
}

// writeError reports a request failure the connection does not survive.
func writeError(w io.Writer, code, message string) error {
	frame, err := frameOf(MsgError, errorHeader{Code: code, Message: message}, nil)
	if err != nil {
		return err
	}
	return WriteFrame(w, frame)
}
