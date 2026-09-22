package pipeline

import (
	"sync"
	"time"
)

// The bounds of the chunk pipeline.
const (
	// Floor is the lowest concurrency the pipeline ever falls to: one
	// outstanding chunk request, which is the sequential download this
	// replaced. It can never be worse than what it replaced, and a
	// transfer at the floor still makes progress on a device with one core
	// and a slow disk.
	Floor = 1
	// Ceiling is the highest it ever rises to. Eight outstanding requests
	// cover a 100 ms round trip at the default average chunk size of 1
	// MiB, which is more than a home link delivers, and beyond that the
	// byte budget rather than the count is what binds. It also stays under
	// what a peer answers at once, so the requests are served rather than
	// queued at the sender.
	Ceiling = 8
	// DefaultStart is where an adaptive pipeline begins, before it has
	// measured anything. It is low enough to be safe on a slow disk and
	// high enough that the first window already overlaps the wire with the
	// disk.
	DefaultStart = 4
	// window is how many chunks are measured before the limit is
	// reconsidered. Fewer would chase noise; more would take a whole small
	// file to react.
	window = 16
)

// Next is the adaptive rule, as a function of what was measured.
//
// The two costs of one chunk are the wire and the disk: netTime is how long
// the chunk spent being asked for and paced, diskTime how long it spent being
// written, hashed and journalled here. Concurrency buys exactly one thing, the
// overlap of the two, so:
//
//   - When the wire dominates, the workers are mostly idle waiting for the
//     peer, and another outstanding request fills that wait with a write. The
//     limit rises.
//   - When the disk dominates, more outstanding requests only park more bytes
//     in memory behind a disk that is already the bottleneck. The limit falls.
//   - Between the two neither helps, so the limit stays where it is.
//
// The wire has to dominate by a factor of two before the limit rises, and only
// has to lose to make it fall: a pipeline that is too wide costs memory and a
// queue at the sender, and one that is too narrow costs only time.
func Next(limit int, netTime, diskTime time.Duration) int {
	switch {
	case netTime > 2*diskTime:
		limit++
	case diskTime > netTime:
		limit--
	}
	return min(max(limit, Floor), Ceiling)
}

// Limiter decides how many chunk requests of one transfer are outstanding. It
// is either pinned, which is what a test and a constrained device ask for, or
// adaptive, which measures every chunk and reconsiders once per window.
//
// One Limiter stands for one device: every folder and every transfer shares
// it, so what it learns about this disk and this link is not re-learned per
// file. It is used from many goroutines at once.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	pinned  bool
	samples int
	// netTime and diskTime are what the chunks of the current window cost.
	netTime, diskTime time.Duration
}

// Adaptive returns a limiter that starts at DefaultStart and moves with what
// it measures.
func Adaptive() *Limiter { return &Limiter{limit: clamp(DefaultStart)} }

// Pinned returns a limiter fixed at n, clamped into [Floor, Ceiling].
// Pinned(1) is the sequential download: one chunk asked for at a time.
func Pinned(n int) *Limiter { return &Limiter{limit: clamp(n), pinned: true} }

// Limit is how many chunk requests may be outstanding now.
func (l *Limiter) Limit() int {
	if l == nil {
		return Floor
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// Pinned reports whether the limit is fixed.
func (l *Limiter) Pinned() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pinned
}

// Observe records what one chunk cost. Once a window of them has been seen the
// limit is reconsidered and the window starts again. A pinned limiter records
// nothing.
func (l *Limiter) Observe(netTime, diskTime time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pinned {
		return
	}
	l.samples++
	l.netTime += netTime
	l.diskTime += diskTime
	if l.samples < window {
		return
	}
	l.limit = Next(l.limit, l.netTime, l.diskTime)
	l.samples, l.netTime, l.diskTime = 0, 0, 0
}

// clamp holds a limit inside the bounds, so no configuration and no
// measurement can put the pipeline outside them.
func clamp(n int) int { return min(max(n, Floor), Ceiling) }
