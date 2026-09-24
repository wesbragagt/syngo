package fakes

import "github.com/wesbragagt/syngo/internal/domain/device"

// Identity is a fixed ports.Identity.
type Identity struct {
	ID         device.ID
	DeviceName string
}

// NewIdentity returns an Identity with the given id and name.
func NewIdentity(id device.ID, name string) Identity {
	return Identity{ID: id, DeviceName: name}
}

// DeviceID returns the configured device ID.
func (i Identity) DeviceID() device.ID { return i.ID }

// Name returns the configured device name.
func (i Identity) Name() string { return i.DeviceName }
