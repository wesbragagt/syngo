package scan_test

import (
	"testing"

	"github.com/wesbragagt/syngo/internal/app/scan"
	"github.com/wesbragagt/syngo/internal/domain/counterfloor"
)

// TestScan_VersionsWrittenBeforeAPeerConfirmsTheFloorAreProvisional covers the
// restore window. The counter floor a restored database hands out is the one
// the backup recorded, so a version written before any peer has been heard
// from may carry a counter the peers already hold for this device. The entry
// says so, and the sync plan is what acts on it.
func TestScan_VersionsWrittenBeforeAPeerConfirmsTheFloorAreProvisional(t *testing.T) {
	e := newEnv(t)
	trust := counterfloor.New()
	e.scanner.SetCounterFloorTrust(trust)

	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})
	if got := e.entry(t, "a.txt"); !got.Provisional {
		t.Fatalf("version %q of a.txt is not provisional, want it marked: no peer has confirmed the floor",
			got.Version)
	}

	// The first manifest a peer sends confirms the floor; from then on the
	// counters this device hands out are known to be above what the peers
	// hold, and nothing is in doubt.
	trust.Verify()
	e.overwrite(t, "a.txt", "beta")
	e.scan(t, scan.Options{})
	if got := e.entry(t, "a.txt"); got.Provisional {
		t.Fatalf("version %q of a.txt is provisional, want the confirmed floor to have cleared it",
			got.Version)
	}
}

// TestScan_AModeChangeDoesNotMakeAVersionProvisional guards the other half of
// the rule: only a change that hands out a counter can have been given one the
// peers already hold. A permission change keeps the vector it found, so the
// entry keeps whatever it already was and no conflict is manufactured for a
// version a peer legitimately built on.
func TestScan_AModeChangeDoesNotMakeAVersionProvisional(t *testing.T) {
	e := newEnv(t)
	trust := counterfloor.New()
	trust.Verify()
	e.scanner.SetCounterFloorTrust(trust)

	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	// A later start, before any peer has been heard from: the floor is in
	// doubt again, but a chmod hands out no counter.
	e.scanner.SetCounterFloorTrust(counterfloor.New())
	if err := e.fs.Chmod(root+"/a.txt", 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	e.scan(t, scan.Options{})

	got := e.entry(t, "a.txt")
	if got.Mode != 0o600 {
		t.Fatalf("mode = %o, want the chmod recorded", got.Mode)
	}
	if got.Provisional {
		t.Fatalf("version %q of a.txt is provisional, want a mode change to leave it alone", got.Version)
	}
}

// TestScan_ADeletionBeforeAPeerConfirmsTheFloorIsProvisional covers the
// tombstone, whose counter the store raises rather than the scanner: a
// deletion is a content change and carries a counter like any other.
func TestScan_ADeletionBeforeAPeerConfirmsTheFloorIsProvisional(t *testing.T) {
	e := newEnv(t)
	trust := counterfloor.New()
	trust.Verify()
	e.scanner.SetCounterFloorTrust(trust)

	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})

	e.scanner.SetCounterFloorTrust(counterfloor.New())
	e.remove(t, "a.txt")
	e.scan(t, scan.Options{})

	got := e.entry(t, "a.txt")
	if !got.Deleted {
		t.Fatalf("a.txt is not a tombstone, want the deletion recorded")
	}
	if !got.Provisional {
		t.Fatalf("tombstone %q of a.txt is not provisional, want it marked", got.Version)
	}
}

// TestScan_WithoutATrustNothingIsProvisional keeps the default the behaviour
// it had before the floor was ever doubted: a scanner nobody wired a trust
// into treats the floor as confirmed.
func TestScan_WithoutATrustNothingIsProvisional(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "alpha")
	e.scan(t, scan.Options{})
	if got := e.entry(t, "a.txt"); got.Provisional {
		t.Fatalf("version %q of a.txt is provisional, want a scanner without a trust to mark nothing",
			got.Version)
	}
}
