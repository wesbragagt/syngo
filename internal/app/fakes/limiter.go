package fakes

import (
	"context"
	"sync"

	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
)

// Limiter is a ports.Limiter that records what it was asked to pass and never
// waits, unless a test installs a gate.
type Limiter struct {
	mu    sync.Mutex
	rate  bandwidth.Rate
	waits []int
	rates []bandwidth.Rate
	// gate blocks every Wait until it is closed, so a test can hold a
	// transfer still and look at it.
	gate chan struct{}
	// waited is signalled after every Wait is recorded.
	waited chan struct{}
}

// NewLimiter returns an unlimited limiter that records its calls.
func NewLimiter() *Limiter {
	return &Limiter{waited: make(chan struct{}, 1024)}
}

// Wait records n and returns at once, or blocks on the gate a test installed.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	l.mu.Lock()
	l.waits = append(l.waits, n)
	gate := l.gate
	l.mu.Unlock()

	select {
	case l.waited <- struct{}{}:
	default:
	}

	if gate == nil {
		return ctx.Err()
	}
	select {
	case <-gate:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetRate records the new rate.
func (l *Limiter) SetRate(r bandwidth.Rate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = r
	l.rates = append(l.rates, r)
}

// Rate returns the rate last set.
func (l *Limiter) Rate() bandwidth.Rate {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// Waits returns the byte counts passed to Wait, in order.
func (l *Limiter) Waits() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.waits...)
}

// Rates returns the rates passed to SetRate, in order.
func (l *Limiter) Rates() []bandwidth.Rate {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bandwidth.Rate(nil), l.rates...)
}

// Block makes every Wait block until the returned function is called.
func (l *Limiter) Block() func() {
	gate := make(chan struct{})
	l.mu.Lock()
	l.gate = gate
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.gate = nil
			l.mu.Unlock()
			close(gate)
		})
	}
}

// Waited reports a signal per recorded Wait, for a test that has to know one
// happened before it does something else.
func (l *Limiter) Waited() <-chan struct{} { return l.waited }
