package tcpwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/chunker"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// ProtocolVersion is the only version this implementation speaks. Version 3
// carries chunk boundaries: chunking is content-defined, so the sender says
// where it cut a file and a chunk request names a byte range rather than an
// index and a chunk size. A version-2 peer derives boundaries from a size and
// would answer a range request with the wrong bytes, so the versions do not
// interoperate and every device of a mesh upgrades together.
const ProtocolVersion = 3

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

// manifestEntry is one entry of a ManifestResponse. Version is the version
// vector of the entry, device id to counter, which is what the receiver needs
// to tell a newer version from a diverged one. It is absent for a path whose
// stored row predates vectors, which reads as the zero vector.
type manifestEntry struct {
	Path       string            `json:"path"`
	Kind       uint8             `json:"kind"`
	Size       int64             `json:"size"`
	MTimeNS    int64             `json:"mtime_ns"`
	Mode       uint32            `json:"mode"`
	Hash       string            `json:"hash"`
	LinkTarget string            `json:"link_target"`
	Deleted    bool              `json:"deleted"`
	ModifiedBy string            `json:"modified_by"`
	Sequence   uint64            `json:"sequence"`
	Version    map[string]uint64 `json:"version,omitempty"`
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

// chunkRequestHeader is the ChunkRequest frame header. The byte range is the
// one the sender announced in its chunk list; neither side derives it from a
// size.
type chunkRequestHeader struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
	Hash     string `json:"hash"`
	Index    int    `json:"index"`
	Offset   int64  `json:"offset"`
	Length   int    `json:"length"`
}

// chunkListRequestHeader is the ChunkListRequest frame header.
type chunkListRequestHeader struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
	Hash     string `json:"hash"`
}

// chunkListReplyHeader is the ChunkListReply frame header. The list itself is
// the payload: a 10 GiB file is ten thousand chunks at the default size, and
// JSON would spend three times the bytes and the parse time on them.
type chunkListReplyHeader struct {
	FolderID string `json:"folder_id"`
	Path     string `json:"path"`
	Count    int    `json:"count"`
}

// chunkEntrySize is how many bytes one chunk takes in a ChunkListReply payload:
// a big-endian uint32 length followed by the 32-byte hash. The offsets and the
// indexes are not sent: chunks are contiguous and in order, so both follow.
const chunkEntrySize = 4 + manifest.HashSize

// encodeChunkList packs a chunk list into a ChunkListReply payload.
func encodeChunkList(chunks []transfer.Chunk) []byte {
	out := make([]byte, 0, len(chunks)*chunkEntrySize)
	for _, c := range chunks {
		out = binary.BigEndian.AppendUint32(out, uint32(c.Length))
		out = append(out, c.Hash[:]...)
	}
	return out
}

// ErrMalformedChunkList reports a ChunkListReply whose payload does not hold
// the announced number of chunks, or whose chunks are not usable.
var ErrMalformedChunkList = errors.New("malformed chunk list")

// decodeChunkList unpacks a ChunkListReply payload, rebuilding the offset and
// the index of every chunk from the order they arrive in. maxChunks is the
// largest list the caller accepts; the count is read before the payload is
// walked, so a peer cannot make this device allocate more than that.
func decodeChunkList(h chunkListReplyHeader, payload []byte, maxChunks int) ([]transfer.Chunk, error) {
	switch {
	case h.Count < 0 || h.Count > maxChunks:
		return nil, fmt.Errorf("%w: %d chunks, the limit is %d", ErrMalformedChunkList, h.Count, maxChunks)
	case len(payload) != h.Count*chunkEntrySize:
		return nil, fmt.Errorf("%w: %d chunks announced, payload has %d bytes",
			ErrMalformedChunkList, h.Count, len(payload))
	}
	chunks := make([]transfer.Chunk, 0, h.Count)
	var offset int64
	for i := range h.Count {
		entry := payload[i*chunkEntrySize:]
		length := int64(binary.BigEndian.Uint32(entry[:4]))
		if length <= 0 || length > chunker.MaxSize {
			return nil, fmt.Errorf("%w: chunk %d is %d bytes", ErrMalformedChunkList, i, length)
		}
		var hash manifest.Hash
		copy(hash[:], entry[4:chunkEntrySize])
		chunks = append(chunks, transfer.Chunk{
			Index: i, Offset: offset, Length: int(length), Hash: hash,
		})
		offset += length
	}
	return chunks, nil
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
			Version:    wireVersion(e.Version),
		})
	}
	return manifestReplyHeader{FolderID: string(m.FolderID), Sequence: m.Sequence, Entries: entries}
}

// wireVersion turns a version vector into the map the wire carries. The zero
// vector is left out of the frame entirely.
func wireVersion(v manifest.Version) map[string]uint64 {
	if v.IsZero() {
		return nil
	}
	out := make(map[string]uint64, len(v.Counters()))
	for _, c := range v.Counters() {
		out[string(c.ID)] = c.N
	}
	return out
}

// DefaultMaxFileSize is the largest file accepted from a peer when a dialer
// is given no limit: one tebibyte.
const DefaultMaxFileSize int64 = 1 << 40

// ErrEntryTooLarge reports a manifest entry announcing a file this device does
// not accept. The whole manifest is refused.
var ErrEntryTooLarge = errors.New("manifest entry announces a file that is too large")

// fromWireManifest converts a received manifest into the domain form. maxSize
// is the largest file the caller accepts; an entry announcing more, or a
// negative size, refuses the whole manifest.
func fromWireManifest(h manifestReplyHeader, maxSize int64) (manifest.Manifest, error) {
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
		if e.Size < 0 || e.Size > maxSize {
			// The size decides how many chunks are allocated and
			// journalled, so it is checked before anything is built
			// from it.
			return manifest.Manifest{}, fmt.Errorf("manifest entry %q: size %d is outside 0..%d: %w",
				e.Path, e.Size, maxSize, ErrEntryTooLarge)
		}
		hash, err := manifest.ParseHash(e.Hash)
		if err != nil {
			return manifest.Manifest{}, fmt.Errorf("manifest entry %q: %w", e.Path, err)
		}
		// A counter naming something that is not a device, or counting
		// zero changes, is not a vector this device can order anything
		// against, so the whole manifest is refused.
		version, err := manifest.NewVersion(wireCounters(e.Version))
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
			Version:    version,
		})
	}
	return m, nil
}

// wireCounters turns the map a peer sent into counters. A duplicate id cannot
// survive JSON decoding, so NewVersion sees at most one counter per device.
func wireCounters(m map[string]uint64) []manifest.Counter {
	if len(m) == 0 {
		return nil
	}
	out := make([]manifest.Counter, 0, len(m))
	for id, n := range m {
		out = append(out, manifest.Counter{ID: device.ID(id), N: n})
	}
	return out
}

// toWireChunkRequest converts a chunk request into its wire form.
func toWireChunkRequest(req ports.ChunkRequest) chunkRequestHeader {
	return chunkRequestHeader{
		FolderID: string(req.FolderID),
		Path:     req.Path,
		Hash:     req.Hash.String(),
		Index:    req.Index,
		Offset:   req.Offset,
		Length:   req.Length,
	}
}

// fromWireChunkRequest converts a received chunk request into the port's form.
func fromWireChunkRequest(h chunkRequestHeader) (ports.ChunkRequest, error) {
	hash, err := manifest.ParseHash(h.Hash)
	if err != nil {
		return ports.ChunkRequest{}, fmt.Errorf("chunk request for %q: %w", h.Path, err)
	}
	return ports.ChunkRequest{
		FolderID: folder.ID(h.FolderID),
		Path:     h.Path,
		Hash:     hash,
		Index:    h.Index,
		Offset:   h.Offset,
		Length:   h.Length,
	}, nil
}
