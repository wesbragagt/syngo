// Package tokenbucket paces byte streams with a token bucket: a rate that
// refills the bucket and a capacity that decides how much of an idle period
// may be spent at once.
package tokenbucket

import (
	"context"
	"sync"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
)

var _ ports.Limiter = (*Limiter)(nil)

// burstFraction is which part of a second of traffic the bucket holds: 100 ms
// at the configured rate. It is what lets a whole chunk through in one go
// without letting an idle minute be spent at once.
const burstFraction = 10

// Limiter is a token bucket. Wait is safe for concurrent use and hands the
// tokens out in no particular order: every caller is asking for the same
// device-wide budget.
type Limiter struct {
	mu      sync.Mutex
	rate    bandwidth.Rate
	burst   float64
	tokens  float64
	last    time.Time
	changed chan struct{}

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// New returns a limiter running at r, with a full bucket.
func New(r bandwidth.Rate) *Limiter {
	return NewWithClock(r, time.Now, sleepFor)
}

// NewWithClock returns a limiter reading the time from now and spending its
// waits in sleep. It is what tests use to make the arithmetic deterministic;
// production code calls New.
func NewWithClock(r bandwidth.Rate, now func() time.Time, sleep func(context.Context, time.Duration) error) *Limiter {
	return &Limiter{
		rate:    r,
		burst:   burstFor(r),
		tokens:  burstFor(r),
		last:    now(),
		changed: make(chan struct{}),
		now:     now,
		sleep:   sleep,
	}
}

// Wait blocks until n bytes may pass, the rate becomes unlimited, or ctx ends.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		l.mu.Lock()
		if l.rate.Unlimited() {
			l.mu.Unlock()
			return nil
		}
		l.refillLocked()
		// The level is allowed to go negative: a chunk bigger than the
		// bucket would never be admitted otherwise. The debt is slept
		// off, which is what makes the average rate come out right.
		l.tokens -= float64(n)
		delay := time.Duration(0)
		if l.tokens < 0 {
			delay = time.Duration(-l.tokens / float64(l.rate) * float64(time.Second))
		}
		changed := l.changed
		l.mu.Unlock()

		if delay <= 0 {
			return nil
		}

		slept := make(chan error, 1)
		go func() { slept <- l.sleep(ctx, delay) }()

		select {
		case err := <-slept:
			if err != nil {
				l.give(n)
				return err
			}
			select {
			case <-changed:
				// The rate moved while this wait was sleeping,
				// so what it slept off was computed against a
				// rate that no longer holds. Give the tokens
				// back and decide again under the new one.
				l.give(n)
			default:
				return nil
			}
		case <-changed:
			l.give(n)
		case <-ctx.Done():
			l.give(n)
			return ctx.Err()
		}
	}
}

// SetRate changes the limit. Waiters wake and decide again under the new rate,
// so lowering it stretches a pending wait and unlimited ends every one of them.
func (l *Limiter) SetRate(r bandwidth.Rate) {
	if r < 0 {
		r = bandwidth.Unlimited
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if r == l.rate {
		return
	}

	// The time spent under the old rate is credited before the new one
	// takes over, so no period is ever accounted at a rate it did not run
	// at.
	l.refillLocked()
	was := l.rate
	l.rate = r
	l.burst = burstFor(r)
	switch {
	case r.Unlimited():
		l.tokens = 0
	case was.Unlimited():
		l.tokens = l.burst
	case l.tokens > l.burst:
		l.tokens = l.burst
	}

	close(l.changed)
	l.changed = make(chan struct{})
}

// Rate returns the limit in force.
func (l *Limiter) Rate() bandwidth.Rate {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// refillLocked credits the time since the last look at the current rate.
func (l *Limiter) refillLocked() {
	now := l.now()
	elapsed := now.Sub(l.last)
	if elapsed <= 0 {
		return
	}
	l.last = now
	l.tokens += elapsed.Seconds() * float64(l.rate)
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}

// give returns the tokens of a wait that was abandoned or is about to be
// decided again, so nothing is charged twice.
func (l *Limiter) give(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens += float64(n)
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}

// burstFor returns the capacity of a bucket running at r: 100 ms of traffic,
// and at least one byte so that a rate of a few bytes per second still moves.
func burstFor(r bandwidth.Rate) float64 {
	if r.Unlimited() {
		return 0
	}
	burst := float64(r) / burstFraction
	if burst < 1 {
		burst = 1
	}
	return burst
}

// sleepFor waits for d, or until ctx ends.
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
