// Package fakes provides in-memory implementations of every port, for use in
// tests of any package.
package fakes

import (
	"sync"
	"time"
)

// Clock is a manually advanced ports.Clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock reading start, or a fixed default if start is zero.
func NewClock(start time.Time) *Clock {
	if start.IsZero() {
		start = time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC)
	}
	return &Clock{now: start}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d and returns the new time.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
