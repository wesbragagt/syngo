package ports

import (
	"context"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

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
	Hello(ctx context.Context) HelloInfo
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
}
