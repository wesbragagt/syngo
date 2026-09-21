package tcpwire_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/adapters/tcpwire"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// identity is a key pair with the device ID derived from it. The test builds
// its own instead of using the identityfile adapter, which it may not import.
type identity struct {
	cert tls.Certificate
	id   device.ID
	name string
}

// newIdentity generates a self-signed ed25519 identity.
func newIdentity(t *testing.T, name string) identity {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "syncthis", Organization: []string{name}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return identity{
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf},
		id:   device.IDFromPublicKey(leaf.RawSubjectPublicKeyInfo),
		name: name,
	}
}

// hello is the Hello an identity announces.
func (i identity) hello() ports.HelloInfo {
	return ports.HelloInfo{
		DeviceID: i.id,
		Name:     i.name,
		Folders:  []ports.FolderAdvert{{ID: folder.ID("docs"), Mode: folder.ModeSendReceive}},
	}
}

// handler is a ports.PeerHandler answering Hello only, which is all the
// transport needs before the sync engine lands.
type handler struct {
	info ports.HelloInfo
}

// Hello returns the configured Hello.
func (h handler) Hello(context.Context) ports.HelloInfo { return h.info }

// Manifest is not reachable in this milestone.
func (h handler) Manifest(context.Context, device.ID, folder.ID) (manifest.Manifest, error) {
	return manifest.Manifest{}, errors.New("not implemented")
}

// Chunk is not reachable in this milestone.
func (h handler) Chunk(context.Context, device.ID, ports.ChunkRequest) (ports.ChunkData, error) {
	return ports.ChunkData{}, errors.New("not implemented")
}

// lookups records the device IDs the listener asked about.
type lookups struct {
	mu    sync.Mutex
	seen  []device.ID
	allow bool
}

// fn returns the callback to hand to Serve.
func (l *lookups) fn() ports.DeviceLookup {
	return func(_ context.Context, id device.ID) bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.seen = append(l.seen, id)
		return l.allow
	}
}

// ids returns the recorded lookups.
func (l *lookups) ids() []device.ID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]device.ID(nil), l.seen...)
}

// startServer binds a listener on loopback and serves it until the test ends.
func startServer(t *testing.T, srv identity, hello ports.HelloInfo, known ports.DeviceLookup, idle time.Duration) *tcpwire.Listener {
	t.Helper()

	ln, err := tcpwire.Listen(srv.cert, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if idle > 0 {
		ln.SetTimeouts(2*time.Second, idle)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ln.Serve(ctx, handler{info: hello}, known) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve() = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return ln
}

// newDialer returns a dialer for an identity with test timings.
func newDialer(id identity) *tcpwire.Dialer {
	d := tcpwire.NewDialer(id.cert, id.hello, nil)
	d.SetTimeouts(time.Second, 3*time.Second, 3*time.Second)
	return d
}

func TestHandshake_KnownPeerAccepted(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	known := &lookups{allow: true}
	ln := startServer(t, server, server.hello(), known.fn(), 0)

	conn, err := newDialer(client).Dial(context.Background(), ln.Addr(), server.id)
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if conn.RemoteID() != server.id {
		t.Fatalf("RemoteID = %s, want %s", conn.RemoteID(), server.id)
	}
	if hello := conn.RemoteHello(); hello.Name != "server" || len(hello.Folders) != 1 {
		t.Fatalf("RemoteHello = %+v, want the server's name and folders", hello)
	}
	if seen := known.ids(); len(seen) != 1 || seen[0] != client.id {
		t.Fatalf("device lookups = %v, want [%s]", seen, client.id)
	}
}

func TestHandshake_UnknownPeerRejectedWithReason(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	ln := startServer(t, server, server.hello(), (&lookups{allow: false}).fn(), 0)
	dialer := newDialer(client)

	// A probe still learns who answers, which is what pairing needs.
	result, err := dialer.Probe(context.Background(), ln.Addr())
	if err != nil {
		t.Fatalf("Probe error: %v", err)
	}
	if result.DeviceID != server.id || result.Name != "server" {
		t.Fatalf("probe = %+v, want the server identity", result)
	}
	if result.Accepted || result.RejectReason != tcpwire.ReasonUnknownDevice {
		t.Fatalf("probe = %+v, want rejected as unknown_device", result)
	}

	_, err = dialer.Dial(context.Background(), ln.Addr(), server.id)
	var rejected *ports.RejectedError
	if !errors.As(err, &rejected) || rejected.Reason != tcpwire.ReasonUnknownDevice {
		t.Fatalf("Dial error = %v, want a rejection", err)
	}
}

func TestDial_ExpectedIDMismatchFails(t *testing.T) {
	server, client, other := newIdentity(t, "server"), newIdentity(t, "client"), newIdentity(t, "other")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 0)

	_, err := newDialer(client).Dial(context.Background(), ln.Addr(), other.id)
	if !errors.Is(err, ports.ErrDeviceMismatch) {
		t.Fatalf("Dial error = %v, want ErrDeviceMismatch", err)
	}
}

func TestHandshake_HelloAnnouncingAnotherDeviceRejected(t *testing.T) {
	server, client, other := newIdentity(t, "server"), newIdentity(t, "client"), newIdentity(t, "other")

	// The server's Hello claims an identity its certificate does not have.
	lying := server.hello()
	lying.DeviceID = other.id
	ln := startServer(t, server, lying, (&lookups{allow: true}).fn(), 0)

	_, err := newDialer(client).Dial(context.Background(), ln.Addr(), server.id)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("Dial error = %v, want the announced id to be refused", err)
	}
}

func TestHandshake_ClientAnnouncingAnotherDeviceRejected(t *testing.T) {
	server, client, other := newIdentity(t, "server"), newIdentity(t, "client"), newIdentity(t, "other")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 0)

	lying := client.hello()
	lying.DeviceID = other.id
	dialer := tcpwire.NewDialer(client.cert, func() ports.HelloInfo { return lying }, nil)
	dialer.SetTimeouts(time.Second, 3*time.Second, 3*time.Second)

	_, err := dialer.Dial(context.Background(), ln.Addr(), server.id)
	var rejected *ports.RejectedError
	if !errors.As(err, &rejected) || rejected.Reason != tcpwire.ReasonIDMismatch {
		t.Fatalf("Dial error = %v, want a rejection with id_mismatch", err)
	}
}

func TestHandshake_OtherProtocolVersionRejected(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 0)

	cfg := tcpwire.ClientTLS(client.cert, server.id)
	conn, err := tls.Dial("tcp", ln.Addr(), cfg)
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	header := `{"protocol_version":2,"device_id":"` + string(client.id) + `","device_name":"client","folders":[]}`
	if err := tcpwire.WriteFrame(conn, tcpwire.Frame{Type: tcpwire.MsgHello, Header: []byte(header)}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	// The server answers with its own Hello first, then the rejection.
	for {
		frame, err := tcpwire.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if frame.Type == tcpwire.MsgHello {
			continue
		}
		if frame.Type != tcpwire.MsgRejected {
			t.Fatalf("frame = %s, want rejected", frame.Type)
		}
		if !strings.Contains(string(frame.Header), tcpwire.ReasonProtocolVersion) {
			t.Fatalf("rejection = %s, want protocol_version", frame.Header)
		}
		return
	}
}

func TestHandshake_WrongALPNFails(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 0)

	cfg := tcpwire.ClientTLS(client.cert, server.id)
	cfg.NextProtos = []string{"http/1.1"}
	conn, err := tls.Dial("tcp", ln.Addr(), cfg)
	if err == nil {
		_ = conn.Close()
		t.Fatal("tls dial with the wrong ALPN succeeded")
	}
}

func TestPing_LatencyPositive(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 0)

	conn, err := newDialer(client).Dial(context.Background(), ln.Addr(), server.id)
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	latency, err := conn.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping error: %v", err)
	}
	if latency <= 0 {
		t.Fatalf("latency = %v, want a positive round trip", latency)
	}
}

func TestIdleTimeout_ClosesAfterSilence(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 100*time.Millisecond)

	// The client must not ping, so the server's read deadline fires.
	dialer := tcpwire.NewDialer(client.cert, client.hello, nil)
	dialer.SetTimeouts(time.Hour, 3*time.Second, 3*time.Second)
	conn, err := dialer.Dial(context.Background(), ln.Addr(), server.id)
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer func() { _ = conn.Close() }()

	time.Sleep(400 * time.Millisecond)
	if _, err := conn.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded after the idle timeout, want a closed connection")
	}
}

func TestDial_NothingListeningFailsAtDialStage(t *testing.T) {
	client := newIdentity(t, "client")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = newDialer(client).Dial(context.Background(), addr, "")
	var connErr *ports.ConnectError
	if !errors.As(err, &connErr) || connErr.Stage != ports.StageDial {
		t.Fatalf("Dial error = %v, want a dial-stage ConnectError", err)
	}
}

func TestResolveListen_RejectsPublicWithoutFlag(t *testing.T) {
	cases := []string{"8.8.8.8:22000", "0.0.0.0:22000", ":22000"}
	for _, listen := range cases {
		if _, err := tcpwire.ResolveListen(listen, "", false); !errors.Is(err, tcpwire.ErrPublicAddress) {
			t.Fatalf("ResolveListen(%q) = %v, want ErrPublicAddress", listen, err)
		}
		got, err := tcpwire.ResolveListen(listen, "", true)
		if err != nil {
			t.Fatalf("ResolveListen(%q, allowPublic) error: %v", listen, err)
		}
		if !strings.HasSuffix(got, ":22000") {
			t.Fatalf("ResolveListen(%q, allowPublic) = %q, want the configured port", listen, got)
		}
	}

	for _, listen := range []string{"127.0.0.1:22000", "192.168.1.5:22000", "100.64.0.2:22000"} {
		if _, err := tcpwire.ResolveListen(listen, "", false); err != nil {
			t.Fatalf("ResolveListen(%q) error: %v", listen, err)
		}
	}
}

func TestResolveListen_LoopbackInterface(t *testing.T) {
	if _, err := net.InterfaceByName("lo"); err != nil {
		t.Skipf("no lo interface: %v", err)
	}

	got, err := tcpwire.ResolveListen("0.0.0.0:22000", "lo", false)
	if err != nil {
		t.Fatalf("ResolveListen error: %v", err)
	}
	if got != "127.0.0.1:22000" {
		t.Fatalf("ResolveListen = %q, want 127.0.0.1:22000", got)
	}

	if _, err := tcpwire.ResolveListen("0.0.0.0:22000", "definitely-not-an-interface", false); err == nil {
		t.Fatal("ResolveListen accepted an interface that does not exist")
	}
}

func TestHandshake_ServerSendsHelloBeforeReadingOne(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 0)

	conn, err := tls.Dial("tcp", ln.Addr(), tcpwire.ClientTLS(client.cert, server.id))
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// The client says nothing: pairing needs the server's identity before
	// the client can pin it, so the server must not wait to be greeted.
	frame, err := tcpwire.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if frame.Type != tcpwire.MsgHello {
		t.Fatalf("frame type = %s, want %s", frame.Type, tcpwire.MsgHello)
	}
	if !strings.Contains(string(frame.Header), string(server.id)) {
		t.Fatalf("hello header = %s, want the server device id", frame.Header)
	}
}

func TestFoldersChanged_FromTheClientIsAcceptedAndIgnored(t *testing.T) {
	server, client := newIdentity(t, "server"), newIdentity(t, "client")
	ln := startServer(t, server, server.hello(), (&lookups{allow: true}).fn(), 0)

	conn, err := tls.Dial("tcp", ln.Addr(), tcpwire.ClientTLS(client.cert, server.id))
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	hello := `{"protocol_version":1,"device_id":"` + string(client.id) + `","device_name":"client","folders":[]}`
	if err := tcpwire.WriteFrame(conn, tcpwire.Frame{Type: tcpwire.MsgHello, Header: []byte(hello)}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if frame, err := tcpwire.ReadFrame(conn); err != nil || frame.Type != tcpwire.MsgHello {
		t.Fatalf("read hello = %v, %v, want a hello", frame.Type, err)
	}

	// The serving side keeps no view of the peer's folders, but the frame
	// may travel either way, so it must not end the connection.
	changed := `{"folders":[{"id":"docs","mode":"sendreceive"}]}`
	if err := tcpwire.WriteFrame(conn, tcpwire.Frame{Type: tcpwire.MsgFoldersChanged, Header: []byte(changed)}); err != nil {
		t.Fatalf("write folders_changed: %v", err)
	}
	if err := tcpwire.WriteFrame(conn, tcpwire.Frame{Type: tcpwire.MsgPing, Header: []byte(`{"nonce":7}`)}); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	frame, err := tcpwire.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if frame.Type != tcpwire.MsgPong {
		t.Fatalf("frame type = %s, want %s: the announcement was answered or broke the connection",
			frame.Type, tcpwire.MsgPong)
	}
	if !strings.Contains(string(frame.Header), "7") {
		t.Fatalf("pong header = %s, want nonce 7", frame.Header)
	}
}
