package tcpwire

import (
	"encoding/json"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
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

// errorHeader is the Error frame header.
type errorHeader struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// toWireHello converts the application's Hello into its wire form.
func toWireHello(h ports.HelloInfo) helloHeader {
	folders := make([]folderAdvert, 0, len(h.Folders))
	for _, f := range h.Folders {
		folders = append(folders, folderAdvert{ID: string(f.ID), Mode: string(f.Mode)})
	}
	return helloHeader{
		ProtocolVersion: ProtocolVersion,
		DeviceID:        string(h.DeviceID),
		DeviceName:      h.Name,
		Folders:         folders,
	}
}

// fromWireHello converts a received Hello into the application's form.
func fromWireHello(h helloHeader) (ports.HelloInfo, error) {
	id, err := device.ParseID(h.DeviceID)
	if err != nil {
		return ports.HelloInfo{}, fmt.Errorf("hello device id: %w", err)
	}
	folders := make([]ports.FolderAdvert, 0, len(h.Folders))
	for _, f := range h.Folders {
		folders = append(folders, ports.FolderAdvert{ID: folder.ID(f.ID), Mode: folder.Mode(f.Mode)})
	}
	return ports.HelloInfo{DeviceID: id, Name: h.DeviceName, Folders: folders}, nil
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
