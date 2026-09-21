package tcpwire

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
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
