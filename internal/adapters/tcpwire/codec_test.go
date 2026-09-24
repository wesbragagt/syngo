package tcpwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// sampleID is a syntactically valid device ID used by the codec tests.
const sampleID = device.ID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

func TestHello_RoundTripPreservesFolders(t *testing.T) {
	want := ports.HelloInfo{
		DeviceID: sampleID,
		Name:     "laptop",
		Folders: []ports.FolderAdvert{
			{ID: folder.ID("docs"), Mode: folder.ModeSendReceive},
			{ID: folder.ID("photos"), Mode: folder.ModeSendOnly},
		},
	}

	got, err := fromWireHello(toWireHello(want))
	if err != nil {
		t.Fatalf("fromWireHello error: %v", err)
	}
	if got.DeviceID != want.DeviceID || got.Name != want.Name || len(got.Folders) != len(want.Folders) {
		t.Fatalf("hello = %+v, want %+v", got, want)
	}
	for i, f := range got.Folders {
		if f != want.Folders[i] {
			t.Fatalf("folder %d = %+v, want %+v", i, f, want.Folders[i])
		}
	}
}

func TestHello_RejectsUnparsableDeviceID(t *testing.T) {
	if _, err := fromWireHello(helloHeader{ProtocolVersion: 1, DeviceID: "nope"}); err == nil {
		t.Fatal("fromWireHello accepted a device id that is not a device id")
	}
}

func TestFrameOf_EncodesHeaderAsJSON(t *testing.T) {
	frame, err := frameOf(MsgPing, nonceHeader{Nonce: 42}, nil)
	if err != nil {
		t.Fatalf("frameOf error: %v", err)
	}
	if frame.Type != MsgPing {
		t.Fatalf("type = %s, want ping", frame.Type)
	}

	var header nonceHeader
	if err := decodeHeader(frame, &header); err != nil {
		t.Fatalf("decodeHeader error: %v", err)
	}
	if header.Nonce != 42 {
		t.Fatalf("nonce = %d, want 42", header.Nonce)
	}
}

func TestFoldersChanged_HeaderRoundTrip(t *testing.T) {
	want := []ports.FolderAdvert{
		{ID: folder.ID("docs"), Mode: folder.ModeSendReceive},
		{ID: folder.ID("photos"), Mode: folder.ModeReceiveOnly},
	}

	frame, err := frameOf(MsgFoldersChanged, foldersChangedHeader{Folders: toWireAdverts(want)}, nil)
	if err != nil {
		t.Fatalf("frameOf error: %v", err)
	}
	if frame.Type != MsgFoldersChanged || frame.Type.String() != "folders_changed" {
		t.Fatalf("type = %s (0x%02x), want folders_changed (0x13)", frame.Type, uint8(frame.Type))
	}

	var header foldersChangedHeader
	if err := decodeHeader(frame, &header); err != nil {
		t.Fatalf("decodeHeader error: %v", err)
	}
	got := fromWireAdverts(header.Folders)
	if len(got) != len(want) {
		t.Fatalf("folders = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("folder %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestFromWireManifest_RejectsAHostileManifest proves the wire layer refuses a
// peer that names paths outside the folder. The whole manifest goes, not the
// single entry: a peer that sends one is not one to take anything from.
func TestFromWireManifest_RejectsAHostileManifest(t *testing.T) {
	hostile := []string{
		"..", "../escape.txt", "../../escape.txt", "a/../../escape.txt",
		"/etc/passwd", "", ".", "./a", "a//b", ".syngo/tmp/x",
		".syngoignore", "a\x00b",
	}
	for _, p := range hostile {
		t.Run(p, func(t *testing.T) {
			header := manifestReplyHeader{
				FolderID: "docs",
				Entries: []manifestEntry{
					{Path: "fine.txt", Kind: 1},
					{Path: p, Kind: 1, Size: 10},
				},
			}
			if _, err := fromWireManifest(header, DefaultMaxFileSize); err == nil {
				t.Fatalf("fromWireManifest accepted the path %q", p)
			}
		})
	}
}

// TestFromWireManifest_RejectsATombstoneOutsideTheFolder covers the delete
// half: a peer must not be able to have a file removed beside the folder.
func TestFromWireManifest_RejectsATombstoneOutsideTheFolder(t *testing.T) {
	header := manifestReplyHeader{
		FolderID: "docs",
		Entries:  []manifestEntry{{Path: "../x", Kind: 1, Deleted: true}},
	}
	if _, err := fromWireManifest(header, DefaultMaxFileSize); err == nil {
		t.Fatal("fromWireManifest accepted a tombstone for a path outside the folder")
	}
}

func TestFromWireManifest_RejectsASymlinkLeavingTheFolder(t *testing.T) {
	for _, target := range []string{"/etc", "../outside", "/tmp/outside", ".syngo"} {
		t.Run(target, func(t *testing.T) {
			header := manifestReplyHeader{
				FolderID: "docs",
				Entries:  []manifestEntry{{Path: "a", Kind: 3, LinkTarget: target}},
			}
			if _, err := fromWireManifest(header, DefaultMaxFileSize); err == nil {
				t.Fatalf("fromWireManifest accepted the link target %q", target)
			}
		})
	}
}

func TestFromWireManifest_KeepsValidEntries(t *testing.T) {
	header := manifestReplyHeader{
		FolderID: "docs",
		Entries: []manifestEntry{
			{Path: "a/b.txt", Kind: 1, Size: 3},
			{Path: "café/résumé.txt", Kind: 1, Size: 4},
			{Path: "a/link", Kind: 3, LinkTarget: "b.txt"},
		},
	}
	m, err := fromWireManifest(header, DefaultMaxFileSize)
	if err != nil {
		t.Fatalf("fromWireManifest error: %v", err)
	}
	if len(m.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(m.Entries))
	}
}

// TestFromWireManifest_RejectsAnAbsurdSize proves a peer cannot decide how
// much memory and how many journal rows this device spends: the size is
// checked before anything is built from it.
func TestFromWireManifest_RejectsAnAbsurdSize(t *testing.T) {
	for _, size := range []int64{1 << 60, 1<<63 - 1, DefaultMaxFileSize + 1, -1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			header := manifestReplyHeader{
				FolderID: "docs",
				Entries:  []manifestEntry{{Path: "huge.bin", Kind: 1, Size: size}},
			}
			_, err := fromWireManifest(header, DefaultMaxFileSize)
			if !errors.Is(err, ErrEntryTooLarge) {
				t.Fatalf("fromWireManifest(size %d) = %v, want ErrEntryTooLarge", size, err)
			}
		})
	}
}

func TestFromWireManifest_AcceptsTheLargestAllowedFile(t *testing.T) {
	header := manifestReplyHeader{
		FolderID: "docs",
		Entries:  []manifestEntry{{Path: "huge.bin", Kind: 1, Size: 4096}},
	}
	if _, err := fromWireManifest(header, 4096); err != nil {
		t.Fatalf("fromWireManifest error: %v", err)
	}
}

// wireDeviceID is the device the vector tests attribute versions to.
const wireDeviceID = sampleID

func TestWireManifest_CarriesVersionVectors(t *testing.T) {
	version, err := manifest.NewVersion([]manifest.Counter{{ID: wireDeviceID, N: 3}})
	if err != nil {
		t.Fatalf("NewVersion error: %v", err)
	}
	m := manifest.New("docs")
	m.Add(manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Size: 3, Version: version})
	m.Add(manifest.Entry{Path: "backfilled.txt", Kind: manifest.KindFile, Size: 3})

	header := toWireManifest(m)
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded manifestReplyHeader
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	back, err := fromWireManifest(decoded, DefaultMaxFileSize)
	if err != nil {
		t.Fatalf("fromWireManifest error: %v", err)
	}

	if got := back.Entries["a.txt"].Version; !got.Equals(version) {
		t.Fatalf("version = %q, want %q", got, version)
	}
	// A path whose row predates vectors carries none, and the field is
	// left out of the frame rather than sent empty.
	if got := back.Entries["backfilled.txt"].Version; !got.IsZero() {
		t.Fatalf("version = %q, want the zero vector", got)
	}
	if strings.Contains(string(encoded), `"version":{}`) {
		t.Fatalf("frame carries an empty version map: %s", encoded)
	}
}

func TestFromWireManifest_RejectsAnUnusableVersion(t *testing.T) {
	cases := map[string]map[string]uint64{
		"not a device id": {"nope": 1},
		"zero counter":    {string(wireDeviceID): 0},
	}
	for name, version := range cases {
		t.Run(name, func(t *testing.T) {
			header := manifestReplyHeader{
				FolderID: "docs",
				Entries: []manifestEntry{
					{Path: "fine.txt", Kind: 1},
					{Path: "a.txt", Kind: 1, Size: 3, Version: version},
				},
			}
			if _, err := fromWireManifest(header, DefaultMaxFileSize); err == nil {
				t.Fatal("fromWireManifest accepted the version")
			}
		})
	}
}
