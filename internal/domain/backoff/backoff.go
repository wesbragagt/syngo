// Package backoff answers how long to wait before trying something again.
package backoff

import "time"

// Policy is a doubling backoff with a floor and a ceiling. The zero Policy
// waits DefaultMin and never more than DefaultMax, so a caller that has no
// opinion still never spins.
type Policy struct {
	// Min is the wait after the first failure.
	Min time.Duration
	// Max is the longest wait, however many failures there have been. A
	// peer that stays unreachable is retried at this interval for ever,
	// which costs one dial per Max and nothing in between.
	Max time.Duration
}

// Default bounds, used for a zero field.
const (
	DefaultMin = time.Second
	DefaultMax = 30 * time.Second
)

// Wait returns how long to wait after the given number of consecutive
// failures. Wait(0) and Wait(1) are both Min, because there has been at most
// one failure to back off from; every further failure doubles the wait until
// it reaches Max.
func (p Policy) Wait(failures int) time.Duration {
	minWait, maxWait := p.bounds()
	if failures < 1 {
		failures = 1
	}
	wait := minWait
	for i := 1; i < failures; i++ {
		if wait >= maxWait {
			return maxWait
		}
		wait *= 2
	}
	return min(wait, maxWait)
}

// bounds returns the policy's floor and ceiling, with the defaults filled in
// and a ceiling that is never below the floor.
func (p Policy) bounds() (time.Duration, time.Duration) {
	minWait, maxWait := p.Min, p.Max
	if minWait <= 0 {
		minWait = DefaultMin
	}
	if maxWait <= 0 {
		maxWait = DefaultMax
	}
	return minWait, max(minWait, maxWait)
}
