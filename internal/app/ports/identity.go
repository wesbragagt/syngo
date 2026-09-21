package ports

import "github.com/wesbragagt/syncthis/internal/domain/device"

// Identity is this device's own identity.
type Identity interface {
	DeviceID() device.ID
	Name() string
}
