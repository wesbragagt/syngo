package tcpwire

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// fuzzConn is a net.Conn that reads a fixed script and records what was
// written. It stands in for a peer that has completed the TLS handshake and
// then sends arbitrary bytes.
type fuzzConn struct {
	r io.Reader
	w bytes.Buffer
}

func (c *fuzzConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fuzzConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *fuzzConn) Close() error                { return nil }
func (c *fuzzConn) LocalAddr() net.Addr         { return fuzzAddr{} }
func (c *fuzzConn) RemoteAddr() net.Addr        { return fuzzAddr{} }
func (c *fuzzConn) SetDeadline(time.Time) error { return nil }

func (c *fuzzConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fuzzConn) SetWriteDeadline(time.Time) error { return nil }

type fuzzAddr struct{}

func (fuzzAddr) Network() string { return "fuzz" }
func (fuzzAddr) String() string  { return "fuzz" }

// fuzzPeerID is the identity the certificate of the scripted peer would yield.
var fuzzPeerID = device.ID(strings.Repeat("B", device.IDLength))

// helloFrameBytes wraps a JSON header in a frame of the given type.
func helloFrameBytes(t MsgType, header string) []byte {
	out := make([]byte, 9, 9+len(header))
	binary.BigEndian.PutUint32(out[0:4], uint32(frameOverhead+len(header)))
	out[4] = byte(t)
	binary.BigEndian.PutUint32(out[5:9], uint32(len(header)))
	return append(out, header...)
}

// FuzzExchangeHello drives the handshake against a peer that sends arbitrary
// bytes after TLS. The invariants are that a Hello is never accepted from a
// peer whose certificate says another device, that an accepted Hello speaks
// the current protocol version, and that this side writes its own Hello first
// whatever the peer sends.
func FuzzExchangeHello(f *testing.F) {
	good := `{"protocol_version":3,"device_id":"` + string(fuzzPeerID) +
		`","device_name":"desktop","folders":[{"id":"photos","mode":"sendreceive"}]}`

	seeds := [][]byte{
		// Valid shapes.
		helloFrameBytes(MsgHello, good),
		helloFrameBytes(MsgRejected, `{"reason":"unknown_device"}`),
		helloFrameBytes(MsgRejected, `{"reason":"protocol_version"}`),
		// Known-bad shapes.
		nil,
		helloFrameBytes(MsgHello, ``),
		helloFrameBytes(MsgHello, `{`),
		helloFrameBytes(MsgHello, `{"protocol_version":1,"device_id":"`+string(fuzzPeerID)+`"}`),
		helloFrameBytes(MsgHello, `{"protocol_version":3,"device_id":"`+strings.Repeat("A", device.IDLength)+`"}`),
		helloFrameBytes(MsgHello, `{"protocol_version":3,"device_id":"nope"}`),
		helloFrameBytes(MsgPing, `{"nonce":1}`),
		helloFrameBytes(MsgRejected, `[]`),
		helloFrameBytes(MsgChunkData, `{}`),
		{0xFF, 0xFF, 0xFF, 0xFF, 0x01},
		{0x00, 0x01, 0x00, 0x00, 0x01},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	local := ports.HelloInfo{
		DeviceID: device.ID(strings.Repeat("A", device.IDLength)),
		Name:     "laptop",
		Folders:  []ports.FolderAdvert{{ID: folder.ID("photos"), Mode: folder.ModeSendReceive}},
	}

	f.Fuzz(func(t *testing.T, script []byte) {
		conn := &fuzzConn{r: bytes.NewReader(script)}
		hello, err := exchangeHello(conn, local, fuzzPeerID)

		// This side always announces itself first, so an unpaired peer
		// can learn who we are however the exchange ends.
		sent, sendErr := ReadFrame(bytes.NewReader(conn.w.Bytes()))
		if sendErr != nil {
			t.Fatalf("the hello this side wrote does not decode: %v", sendErr)
		}
		if sent.Type != MsgHello {
			t.Fatalf("this side wrote %s first, want hello", sent.Type)
		}

		if err != nil {
			// A refused exchange yields nothing a caller could
			// mistake for an authenticated peer.
			if hello.DeviceID != "" || len(hello.Folders) != 0 {
				t.Fatalf("a failed exchange returned the device id %q", hello.DeviceID)
			}
			return
		}

		if hello.DeviceID != fuzzPeerID {
			t.Fatalf("accepted a hello announcing %q from a certificate of %q", hello.DeviceID, fuzzPeerID)
		}
		if _, err := device.ParseID(string(hello.DeviceID)); err != nil {
			t.Fatalf("accepted hello holds the invalid device id %q: %v", hello.DeviceID, err)
		}
		// An accepted hello consumed exactly one frame, and that frame
		// was within the pre-handshake limit.
		frame, err := readFrameLimit(bytes.NewReader(script), MaxHelloFrame)
		if err != nil {
			t.Fatalf("an accepted exchange re-reads as %v", err)
		}
		if frame.Type != MsgHello {
			t.Fatalf("an accepted exchange started with %s, want hello", frame.Type)
		}
	})
}
