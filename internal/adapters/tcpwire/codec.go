package tcpwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// ProtocolVersion is the only version this implementation speaks.
const ProtocolVersion = 1

// Reasons a connection is rejected during the handshake.
const (
	ReasonUnknownDevice   = "unknown_device"
	ReasonProtocolVersion = "protocol_version"
	ReasonIDMismatch      = "id_mismatch"
)

// helloHeader is the Hello frame header.
type helloHeader struct {
	ProtocolVersion int            `json:"protocol_version"`
	DeviceID        string         `json:"device_id"`
	DeviceName      string         `json:"device_name"`
	Folders         []folderAdvert `json:"folders"`
}

// folderAdvert is one folder announced in a Hello.
type folderAdvert struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

// rejectedHeader is the Rejected frame header.
type rejectedHeader struct {
	Reason string `json:"reason"`
}

// nonceHeader is the Ping and Pong frame header.
type nonceHeader struct {
	Nonce uint64 `json:"nonce"`
}

// Error codes carried by an Error frame that answers a request. They mirror
// the sentinel errors of the transport port, so a caller can test them with
// errors.Is after the round trip.
const (
	CodeFolderUnknown     = "folder_unknown"
	CodeFolderReceiveOnly = "folder_receive_only"
	CodePaused            = "paused"
	CodeUnknownDevice     = "unknown_device"
	CodeInternal          = "internal"
	CodeBadRequest        = "bad_request"
)

// errorCode names the wire code for an application error.
func errorCode(err error) string {
	switch {
	case errors.Is(err, ports.ErrFolderUnknown):
		return CodeFolderUnknown
	case errors.Is(err, ports.ErrFolderReceiveOnly):
		return CodeFolderReceiveOnly
	case errors.Is(err, ports.ErrPaused):
		return CodePaused
	default:
		return CodeInternal
	}
}

// errorSentinel maps a received error code back to the port sentinel, or nil
// when the code has none.
func errorSentinel(code string) error {
	switch code {
	case CodeFolderUnknown:
		return ports.ErrFolderUnknown
	case CodeFolderReceiveOnly:
		return ports.ErrFolderReceiveOnly
	case CodePaused:
		return ports.ErrPaused
	default:
		return nil
	}
}

// errorHeader is the Error frame header.
type errorHeader struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// foldersChangedHeader is the unsolicited FoldersChanged frame header. It
// replaces the folder list the peer announced in its Hello.
type foldersChangedHeader struct {
	Folders []folderAdvert `json:"folders"`
}

// toWireAdverts converts announced folders into their wire form.
func toWireAdverts(adverts []ports.FolderAdvert) []folderAdvert {
	out := make([]folderAdvert, 0, len(adverts))
	for _, f := range adverts {
		out = append(out, folderAdvert{ID: string(f.ID), Mode: string(f.Mode)})
	}
	return out
}

// fromWireAdverts converts received folder announcements into the port's form.
func fromWireAdverts(adverts []folderAdvert) []ports.FolderAdvert {
	out := make([]ports.FolderAdvert, 0, len(adverts))
	for _, f := range adverts {
		out = append(out, ports.FolderAdvert{ID: folder.ID(f.ID), Mode: folder.Mode(f.Mode)})
	}
	return out
}

// toWireHello converts the application's Hello into its wire form.
func toWireHello(h ports.HelloInfo) helloHeader {
	return helloHeader{
		ProtocolVersion: ProtocolVersion,
		DeviceID:        string(h.DeviceID),
		DeviceName:      h.Name,
		Folders:         toWireAdverts(h.Folders),
	}
}

// fromWireHello converts a received Hello into the application's form.
func fromWireHello(h helloHeader) (ports.HelloInfo, error) {
	id, err := device.ParseID(h.DeviceID)
	if err != nil {
		return ports.HelloInfo{}, fmt.Errorf("hello device id: %w", err)
	}
	return ports.HelloInfo{DeviceID: id, Name: h.DeviceName, Folders: fromWireAdverts(h.Folders)}, nil
}

// frameOf builds a frame whose header is the JSON encoding of h.
func frameOf(t MsgType, h any, payload []byte) (Frame, error) {
	encoded, err := json.Marshal(h)
	if err != nil {
		return Frame{}, fmt.Errorf("encode %s header: %w", t, err)
	}
	return Frame{Type: t, Header: encoded, Payload: payload}, nil
}

// decodeHeader decodes the JSON header of f into out.
func decodeHeader(f Frame, out any) error {
	if err := json.Unmarshal(f.Header, out); err != nil {
		return fmt.Errorf("decode %s header: %w", f.Type, err)
	}
	return nil
}

// manifestRequestHeader is the ManifestRequest frame header.
type manifestRequestHeader struct {
	FolderID string `json:"folder_id"`
}

// manifestEntry is one entry of a ManifestResponse. SyncedHash is the version
// the sender last agreed on with a peer, which is what the receiver needs to
// tell a newer version from a diverged one.
type manifestEntry struct {
	Path       string `json:"path"`
	Kind       uint8  `json:"kind"`
	Size       int64  `json:"size"`
	MTimeNS    int64  `json:"mtime_ns"`
	Mode       uint32 `json:"mode"`
	Hash       string `json:"hash"`
	LinkTarget string `json:"link_target"`
	Deleted    bool   `json:"deleted"`
	ModifiedBy string `json:"modified_by"`
	Sequence   uint64 `json:"sequence"`
	SyncedHash string `json:"synced_hash"`
}

// manifestReplyHeader is the ManifestResponse frame header.
type manifestReplyHeader struct {
	FolderID string          `json:"folder_id"`
	Sequence uint64          `json:"sequence"`
	Entries  []manifestEntry `json:"entries"`
}

// manifestChangedHeader is the unsolicited ManifestChanged frame header.
type manifestChangedHeader struct {
	FolderID string `json:"folder_id"`
	Sequence uint64 `json:"sequence"`
}

// chunkRequestHeader is the ChunkRequest frame header.
type chunkRequestHeader struct {
	FolderID  string `json:"folder_id"`
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	Index     int    `json:"index"`
	ChunkSize int    `json:"chunk_size"`
}

// chunkDataHeader is the ChunkData frame header; the payload is the chunk.
type chunkDataHeader struct {
	FolderID  string `json:"folder_id"`
	Path      string `json:"path"`
	Index     int    `json:"index"`
	Offset    int64  `json:"offset"`
	Length    int    `json:"length"`
	ChunkHash string `json:"chunk_hash"`
}

// chunkErrorHeader is the ChunkError frame header.
type chunkErrorHeader struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
	Index    int    `json:"index"`
	Reason   string `json:"reason"`
}

// toWireManifest converts a manifest into its wire form.
func toWireManifest(m manifest.Manifest) manifestReplyHeader {
	entries := make([]manifestEntry, 0, len(m.Entries))
	for _, e := range m.Sorted() {
		entries = append(entries, manifestEntry{
			Path:       e.Path,
			Kind:       uint8(e.Kind),
			Size:       e.Size,
			MTimeNS:    e.ModTime.UnixNano(),
			Mode:       e.Mode,
			Hash:       e.Hash.String(),
			LinkTarget: e.LinkTarget,
			Deleted:    e.Deleted,
			ModifiedBy: string(e.ModifiedBy),
			Sequence:   e.Sequence,
			SyncedHash: e.SyncedHash.String(),
		})
	}
	return manifestReplyHeader{FolderID: string(m.FolderID), Sequence: m.Sequence, Entries: entries}
}

// fromWireManifest converts a received manifest into the domain form.
func fromWireManifest(h manifestReplyHeader) (manifest.Manifest, error) {
	m := manifest.New(folder.ID(h.FolderID))
	m.Sequence = h.Sequence
	for _, e := range h.Entries {
		// A peer may only name paths inside the folder. One that does
		// otherwise is not a peer to take anything from, so the whole
		// manifest is refused rather than the single entry dropped.
		if err := manifest.ValidRelPath(e.Path); err != nil {
			return manifest.Manifest{}, err
		}
		if manifest.Kind(e.Kind) == manifest.KindSymlink && !e.Deleted {
			if err := manifest.ValidLinkTarget(e.Path, e.LinkTarget); err != nil {
				return manifest.Manifest{}, err
			}
		}
		hash, err := manifest.ParseHash(e.Hash)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("manifest entry %q: %w", e.Path, err)
		}
		// A peer that predates the field sends nothing, which parses to
		// the zero hash and means "no baseline known".
		synced, err := manifest.ParseHash(e.SyncedHash)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("manifest entry %q: %w", e.Path, err)
		}
		m.Add(manifest.Entry{
			Path:       e.Path,
			Kind:       manifest.Kind(e.Kind),
			Size:       e.Size,
			ModTime:    time.Unix(0, e.MTimeNS).UTC(),
			Mode:       e.Mode,
			Hash:       hash,
			LinkTarget: e.LinkTarget,
			Deleted:    e.Deleted,
			ModifiedBy: device.ID(e.ModifiedBy),
			Sequence:   e.Sequence,
			SyncedHash: synced,
		})
	}
	return m, nil
}

// toWireChunkRequest converts a chunk request into its wire form.
func toWireChunkRequest(req ports.ChunkRequest) chunkRequestHeader {
	return chunkRequestHeader{
		FolderID:  string(req.FolderID),
		Path:      req.Path,
		Hash:      req.Hash.String(),
		Index:     req.Index,
		ChunkSize: req.ChunkSize,
	}
}

// fromWireChunkRequest converts a received chunk request into the port's form.
func fromWireChunkRequest(h chunkRequestHeader) (ports.ChunkRequest, error) {
	hash, err := manifest.ParseHash(h.Hash)
	if err != nil {
		return ports.ChunkRequest{}, fmt.Errorf("chunk request for %q: %w", h.Path, err)
	}
	return ports.ChunkRequest{
		FolderID:  folder.ID(h.FolderID),
		Path:      h.Path,
		Hash:      hash,
		Index:     h.Index,
		ChunkSize: h.ChunkSize,
	}, nil
}
