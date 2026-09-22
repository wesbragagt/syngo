// Package pipeline decides how much of a transfer runs at once: how many bytes
// may be in flight before a fetcher has to wait, and how many chunk requests
// are worth having outstanding. Both are rules rather than policies, so they
// live here: no clock, no I/O, nothing to mock.
package pipeline

import (
	"context"
	"sync"
)

// DefaultBudget is how many bytes of fetched chunk content one device holds in
// memory before a fetcher waits. It is the memory bound of the whole download
// side: every chunk is charged against it from before it is asked for until
// after it has been written and hashed, so a slow disk or a paced upload stops
// the fetchers instead of letting bytes pile up. 64 MiB is a few dozen chunks
// at the default average of 1 MiB, which is enough to keep any realistic link
// busy, and it is a number a 512 MiB single-board device can afford.
const DefaultBudget = 64 << 20

// Budget is a byte semaphore. Acquire takes n bytes out of it and blocks until
// they are there; Release puts them back. It is the backpressure: what is in
// flight can never exceed the budget, whatever the concurrency limit says.
//
// A zero Budget, and a nil one, are unlimited: Acquire only checks the
// context. That is what pins a transfer to the old behaviour for a test.
type Budget struct {
	limit int64

	mu   sync.Mutex
	used int64
	// wake is closed and replaced on every release, so every waiter is
	// woken rather than one of them: a waiter that still does not fit
	// blocks again on the new channel, and one that does proceeds.
	wake chan struct{}
}

// NewBudget returns a budget of limit bytes. Zero or less is unlimited.
func NewBudget(limit int64) *Budget {
	return &Budget{limit: limit, wake: make(chan struct{})}
}

// Limit returns how many bytes may be in flight, zero when unlimited.
func (b *Budget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

// Acquire blocks until n bytes are free, or ctx ends. A request for more than
// the whole budget is charged the whole budget rather than deadlocking: one
// chunk always has to be allowed through, however large it is.
func (b *Budget) Acquire(ctx context.Context, n int) error {
	if b == nil || b.limit <= 0 || n <= 0 {
		return ctx.Err()
	}
	want := min(int64(n), b.limit)
	for {
		b.mu.Lock()
		if b.used+want <= b.limit {
			b.used += want
			b.mu.Unlock()
			return nil
		}
		wake := b.wake
		b.mu.Unlock()

		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release puts n bytes back. It must be called with exactly what Acquire was
// called with; a release without an acquire is refused rather than lending the
// budget bytes it does not have.
func (b *Budget) Release(n int) {
	if b == nil || b.limit <= 0 || n <= 0 {
		return
	}
	give := min(int64(n), b.limit)

	b.mu.Lock()
	b.used = max(b.used-give, 0)
	close(b.wake)
	b.wake = make(chan struct{})
	b.mu.Unlock()
}

// InFlight reports how many bytes are held right now.
func (b *Budget) InFlight() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
