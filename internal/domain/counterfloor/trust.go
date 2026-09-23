// Package counterfloor says whether this device's version counter floor can
// be believed.
//
// The floor is the highest counter this device has ever handed out or seen a
// peer hold for it, and every version it writes is raised above it. What the
// floor cannot do on its own is survive a restore: a database put back from a
// backup carries the floor the backup recorded, which the peers have long
// passed. Only a peer can say how far past, and it says so by sending a
// manifest holding counters of this device (see the puller's adoptCounters).
//
// Until that has happened at least once in the running process, the floor is
// unverified: it may be anywhere below the truth. Versions written while it is
// unverified are marked provisional on the entry, and the sync plan refuses to
// let a peer version overwrite one of them on the strength of the vectors
// alone. A restore is not detectable from the database itself, because the
// database is the thing that was restored, so the question is asked per
// process run rather than per database: every start is treated as possibly
// following a restore, which costs nothing when none happened.
package counterfloor

import "sync/atomic"

// Trust records whether the counter floor of this device has been confirmed
// against a peer since the process started. The zero value is unverified,
// which is what every start begins as. It is safe for concurrent use: the
// scanner reads it on every scan and the puller writes it on every first
// contact.
//
// A nil *Trust reads as verified. It is what a caller that does not track the
// question at all holds, and it leaves the behaviour of such a caller exactly
// as it was before the floor was ever doubted.
type Trust struct {
	verified atomic.Bool
}

// New returns an unverified trust.
func New() *Trust { return &Trust{} }

// Verify records that a peer has confirmed the floor. It is idempotent.
func (t *Trust) Verify() {
	if t == nil {
		return
	}
	t.verified.Store(true)
}

// Verified reports whether a peer has confirmed the floor.
func (t *Trust) Verified() bool { return t == nil || t.verified.Load() }
