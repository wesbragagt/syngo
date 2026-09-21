package e2edocker

import (
	"fmt"
	"testing"
)

// TestMesh_ThreeDevicesConverge is the base case the rest of the suite builds
// on: three devices paired with each other, each writing files nobody else
// touches, all of them ending up with the same folder.
func TestMesh_ThreeDevicesConverge(t *testing.T) {
	c := Start(t, 3)

	const perDevice = 20
	for _, dev := range c.Devices {
		for i := 0; i < perDevice; i++ {
			// Sizes from empty to five chunks, so the run covers a
			// file that needs no transfer and files that need
			// several.
			size := (i % 6) * chunkSize
			dev.WriteFile(fmt.Sprintf("%s/f%02d.bin", dev.Name, i), payload(size, byte(i)))
		}
	}

	c.WaitConverged()
	c.CheckInvariants()

	for _, dev := range c.Devices {
		if got := len(dev.Tree()); got != perDevice*len(c.Devices)+len(c.Devices) {
			t.Errorf("%s holds %d entries, want %d files and %d directories",
				dev.Name, got, perDevice*len(c.Devices), len(c.Devices))
		}
		for _, peer := range dev.Status().Peers {
			if !peer.Connected {
				t.Errorf("%s: peer %s is not connected", dev.Name, peer.Name)
			}
		}
	}
}
