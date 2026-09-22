package folder_test

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

const (
	devA = device.ID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	devB = device.ID("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	devC = device.ID("CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
)

func TestAccess_AllowsOnlyTheDevicesItNames(t *testing.T) {
	access := folder.NewAccess([]device.ID{devA, devB})

	for _, id := range []device.ID{devA, devB} {
		if !access.Allows(id) {
			t.Errorf("Allows(%s) = false, want true", id.Short())
		}
	}
	if access.Allows(devC) {
		t.Error("a device that was never granted is allowed")
	}
}

func TestAccess_ZeroValueAllowsNobody(t *testing.T) {
	var access folder.Access

	if !access.Empty() {
		t.Error("the zero access set is not empty")
	}
	if access.Allows(devA) {
		t.Error("the zero access set allows a device")
	}
	if len(access.Devices()) != 0 {
		t.Errorf("Devices() = %v, want none", access.Devices())
	}
}

func TestAccess_EmptyListAllowsNobody(t *testing.T) {
	access := folder.NewAccess(nil)

	if access.Allows(devA) {
		t.Error("an empty access set allows a device")
	}
	if !access.Empty() {
		t.Error("an empty access set reports itself non-empty")
	}
}

func TestAccess_EmptyDeviceIDIsNeverAllowed(t *testing.T) {
	// An unauthenticated caller has no device ID. It must not become a
	// member of the set by naming nothing.
	access := folder.NewAccess([]device.ID{"", devA})

	if access.Allows("") {
		t.Error("the empty device id is allowed")
	}
	if got := access.Devices(); len(got) != 1 || got[0] != devA {
		t.Errorf("Devices() = %v, want [%s]", got, devA)
	}
}

func TestAccess_DevicesAreSortedAndDeduplicated(t *testing.T) {
	access := folder.NewAccess([]device.ID{devC, devA, devC})

	got := access.Devices()
	want := []device.ID{devA, devC}
	if len(got) != len(want) {
		t.Fatalf("Devices() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Devices() = %v, want %v", got, want)
		}
	}
}

func TestAccess_IsACopyOfWhatItWasBuiltFrom(t *testing.T) {
	ids := []device.ID{devA}
	access := folder.NewAccess(ids)
	ids[0] = devB

	if !access.Allows(devA) || access.Allows(devB) {
		t.Error("changing the slice changed the access set")
	}
}
