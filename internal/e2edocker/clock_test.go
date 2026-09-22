package e2edocker

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/conflict"
)

// conflictName is the shape of a conflict copy's name: the stem, the marker,
// the date and time the conflict was noticed and the short id of the peer it
// was noticed with. It is matched to show that a file whose own timestamps are
// years out still gets a name built from the clock, not from the file.
var conflictName = regexp.MustCompile(`^(.*)\.sync-conflict-(\d{8}-\d{6})-([A-Z2-7]{7})(\..*)?$`)

// TestClock_MtimeSkewDoesNotDriveDecisions gives one device's files timestamps
// from another decade. What a device decides about two versions of a file
// comes from their content, so a file dated two years from now must not win
// over a newer one dated three years ago, and the timestamps themselves must
// still cross unchanged.
func TestClock_MtimeSkewDoesNotDriveDecisions(t *testing.T) {
	c := Start(t, 2)
	a, b := c.Device(0), c.Device(1)

	future := time.Now().AddDate(2, 0, 0).Truncate(time.Second)
	past := time.Now().AddDate(-3, 0, 0).Truncate(time.Second)

	fromA := payload(2*chunkSize, 1)
	a.WriteFile("f.bin", fromA)
	a.Touch("f.bin", future)
	c.WaitConverged()
	if got := b.ModTime("f.bin"); absDuration(got.Sub(future)) > time.Second {
		t.Errorf("b holds f.bin dated %s, want the sender's %s", got, future)
	}

	// The device holding the file dated in the future takes a version
	// dated three years ago, because that is the newer version of the
	// file and the dates are not what says so.
	fromB := payload(3*chunkSize+11, 2)
	b.WriteFile("f.bin", fromB)
	b.Touch("f.bin", past)
	c.WaitConverged()
	c.CheckInvariants()

	if got := a.ReadFile("f.bin"); !equalBytes(got, fromB) {
		t.Errorf("a holds f.bin as %s, want b's newer version %s", hashOf(got), hashOf(fromB))
	}
	if got := a.ModTime("f.bin"); absDuration(got.Sub(past)) > time.Second {
		t.Errorf("a holds f.bin dated %s, want b's %s", got, past)
	}

	// A conflict between two versions, one of them dated years out, is
	// still named after the clock of the device that noticed it.
	c.requireNetAdmin()
	c.Partition(0, 1)

	skewed := payload(chunkSize+5, 3)
	fresh := payload(chunkSize+9, 4)
	fromLog := a.LogSize()
	a.WriteFile("both.bin", skewed)
	a.Touch("both.bin", future)
	a.WaitScanRecorded(fromLog)
	fromLog = b.LogSize()
	b.WriteFile("both.bin", fresh)
	b.WaitScanRecorded(fromLog)

	c.Heal()
	c.WaitConverged()
	c.CheckInvariants()

	winner, loser := skewed, fresh
	if hashOf(fresh) > hashOf(skewed) {
		winner, loser = fresh, skewed
	}
	for _, dev := range c.Devices {
		if got := dev.ReadFile("both.bin"); !equalBytes(got, winner) {
			t.Errorf("%s holds both.bin as %s, want the greater hash %s",
				dev.Name, hashOf(got), hashOf(winner))
		}
		copies := conflictCopies(dev.Tree())
		if len(copies) != 1 {
			t.Errorf("%s holds %d conflict copies, want one: %v", dev.Name, len(copies), copies)
			continue
		}
		if got := dev.ReadFile(copies[0]); !equalBytes(got, loser) {
			t.Errorf("%s: the copy %s holds %s, want the losing version %s",
				dev.Name, copies[0], hashOf(got), hashOf(loser))
		}
		match := conflictName.FindStringSubmatch(copies[0])
		if match == nil {
			t.Errorf("%s: the copy %s is not named the way a conflict copy is", dev.Name, copies[0])
			continue
		}
		at, err := time.Parse("20060102-150405", match[2])
		if err != nil {
			t.Errorf("%s: the copy %s carries %q, which is no time: %v", dev.Name, copies[0], match[2], err)
			continue
		}
		// The name says when the conflict was noticed, which is now,
		// not when either version claims to have been written.
		if absDuration(time.Since(at)) > time.Hour {
			t.Errorf("%s: the copy %s is dated %s, want the time the conflict was noticed",
				dev.Name, copies[0], at)
		}
		if !strings.Contains(copies[0], conflict.Marker) {
			t.Errorf("%s: the copy %s carries no conflict marker", dev.Name, copies[0])
		}
	}
}

// absDuration is the absolute value of a duration, for a comparison that does
// not care which side is ahead.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
