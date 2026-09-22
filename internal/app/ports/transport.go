package ports

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Transport errors. Adapters wrap these so callers can use errors.Is.
var (
	// ErrPeerUnreachable reports that a peer could not be contacted or
	// refused the connection.
	ErrPeerUnreachable = errors.New("peer unreachable")
	// ErrDeviceMismatch reports that the device answering at an address is
	// not the one the caller expected.
	ErrDeviceMismatch = errors.New("device id does not match the expected one")
	// ErrFolderUnknown reports a request for a folder the peer does not
	// have configured.
	ErrFolderUnknown = errors.New("folder is not configured")
	// ErrFolderReceiveOnly reports a request for a folder the peer only
	// receives into, so it never serves its contents.
	ErrFolderReceiveOnly = errors.New("folder is receive-only")
	// ErrPaused reports a request for a folder the peer has paused.
	ErrPaused = errors.New("folder is paused")
)

// Stages of a connection attempt, as reported by ConnectError.
const (
	StageDial      = "dial"
	StageTLS       = "tls"
	StageHandshake = "handshake"
	StageTimeout   = "timeout"
)

// ConnectError reports how far a connection attempt got before it failed, so
// callers can tell "nothing listening" from "wrong identity".
type ConnectError struct {
	Stage string
	Err   error
}

// Error implements the error interface.
func (e *ConnectError) Error() string { return fmt.Sprintf("%s: %v", e.Stage, e.Err) }

// Unwrap returns the underlying failure.
func (e *ConnectError) Unwrap() error { return e.Err }

// Is reports that every connection failure is a peer that cannot be reached.
func (e *ConnectError) Is(target error) bool { return target == ErrPeerUnreachable }

// RejectedError reports that the connection was refused after the Hello
// exchange, by the peer or by this side. Reason is a protocol reason such as
// "unknown_device".
type RejectedError struct {
	Reason string
	// Detail says what this side found wrong, and is set only when this
	// side is the one refusing. It replaces the message about a peer that
	// refused us, which would name the wrong side.
	Detail string
}

// Error implements the error interface.
func (e *RejectedError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return "peer rejected the connection: " + e.Reason
}

// Is reports that a rejection is also a peer that cannot be reached, so
// callers that only care about the outcome can test one sentinel.
func (e *RejectedError) Is(target error) bool { return target == ErrPeerUnreachable }

// FolderAdvert is one folder a peer announces in its Hello.
type FolderAdvert struct {
	ID   folder.ID
	Mode folder.Mode
}

// HelloInfo is the identity and folder list a peer announces on connect.
type HelloInfo struct {
	DeviceID device.ID
	Name     string
	Folders  []FolderAdvert
}

// ChunkRequest asks a peer for one chunk of a file.
type ChunkRequest struct {
	FolderID  folder.ID
	Path      string
	Hash      manifest.Hash
	Index     int
	ChunkSize int
}

// ChunkData is one chunk of file content returned by a peer.
type ChunkData struct {
	Index     int
	Offset    int64
	Data      []byte
	ChunkHash manifest.Hash
}

// ChunkErrorReason explains why a peer refused a chunk request.
type ChunkErrorReason string

// Chunk error reasons.
const (
	ChunkNotFound          ChunkErrorReason = "not_found"
	ChunkHashChanged       ChunkErrorReason = "hash_changed"
	ChunkFolderUnknown     ChunkErrorReason = "folder_unknown"
	ChunkFolderReceiveOnly ChunkErrorReason = "folder_receive_only"
	ChunkPaused            ChunkErrorReason = "paused"
)

// ChunkError is returned by RequestChunk when the peer refuses the request.
type ChunkError struct {
	FolderID folder.ID
	Path     string
	Index    int
	Reason   ChunkErrorReason
}

// Error implements the error interface.
func (e *ChunkError) Error() string {
	return fmt.Sprintf("chunk %d of %s/%s refused: %s", e.Index, e.FolderID, e.Path, e.Reason)
}

// PeerConn is an established connection to a peer, driven by the client side.
type PeerConn interface {
	RemoteID() device.ID
	RemoteHello() HelloInfo
	Ping(ctx context.Context) (time.Duration, error)
	RequestManifest(ctx context.Context, id folder.ID) (manifest.Manifest, error)
	// RequestChunk returns a *ChunkError when the peer refuses the request.
	RequestChunk(ctx context.Context, req ChunkRequest) (ChunkData, error)
	// Notifications reports folders the peer says have changed.
	Notifications() <-chan folder.ID
	// Done is closed once the connection has ended, whether it failed or
	// was closed here. A caller watching it notices a peer that went away
	// without waiting for its next request to time out.
	Done() <-chan struct{}
	Close() error
}

// ProbeResult is what a one-shot connection attempt learned about a peer.
type ProbeResult struct {
	DeviceID     device.ID
	Name         string
	Accepted     bool
	RejectReason string
	Latency      time.Duration
}

// Dialer opens connections to peers.
type Dialer interface {
	// Dial connects to addr. If expect is non-empty and the peer's certificate
	// derives a different device ID, the connection fails.
	Dial(ctx context.Context, addr string, expect device.ID) (PeerConn, error)
	// Probe connects only far enough to learn the peer's identity.
	Probe(ctx context.Context, addr string) (ProbeResult, error)
}

// PeerHandler serves incoming peer requests. It is implemented by the
// application layer and called by the listener adapter.
type PeerHandler interface {
	// Hello is what this device announces to peer. The folder list is the
	// one peer is authorized for, so the handshake never names a folder
	// the peer may not have. An empty peer id names nobody and is
	// answered with no folders.
	Hello(ctx context.Context, peer device.ID) HelloInfo
	Manifest(ctx context.Context, peer device.ID, id folder.ID) (manifest.Manifest, error)
	Chunk(ctx context.Context, peer device.ID, req ChunkRequest) (ChunkData, error)
}

// DeviceLookup reports whether a device is known and allowed to connect.
type DeviceLookup func(ctx context.Context, id device.ID) bool

// Listener accepts peer connections.
type Listener interface {
	Serve(ctx context.Context, h PeerHandler, known DeviceLookup) error
	// Addr returns the bound address, or "" before Serve has bound.
	Addr() string
	// Broadcast tells every connected peer that a folder changed.
	Broadcast(id folder.ID, seq uint64)
	// AnnounceFolders tells every connected peer which folders this device
	// serves now, so a peer learns about a folder added, removed or given
	// another mode while the connection is up, without reconnecting. Each
	// peer is told only about the folders it is authorized for, which the
	// listener asks the handler for; folders is what this device serves at
	// all, and never widens what a peer is told.
	AnnounceFolders(folders []FolderAdvert)
	// RetainDevices closes the connections of every peer that is not in
	// the list, so a device removed from the table stops being served
	// without waiting for it to hang up.
	RetainDevices(ids []device.ID)
}

// BoundListener is a Listener that owns a bound socket, so whoever bound it
// can give it back.
type BoundListener interface {
	Listener
	// Close releases the socket. Serve returns shortly after.
	Close() error
}

// Binder binds the port peers connect to. It is what lets the application
// rebind after the address it was listening on went away, without knowing
// anything about sockets or interfaces.
type Binder interface {
	// Resolve returns the address to bind now. It is asked before every
	// bind, so an interface that came back with another address is bound
	// at its new one.
	Resolve() (string, error)
	// Bind binds addr. The first bind a daemon makes decides whether it
	// starts at all, so its error is reported as it is; every later one is
	// a condition to wait out, whatever it says.
	Bind(addr string) (BoundListener, error)
	// Held reports whether addr is still configured on this machine. A
	// listening socket survives the interface under it disappearing, so
	// this is the only way to notice that nothing can reach it any more.
	Held(addr string) bool
}
