package fakes

import (
	"sync"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// Events is a ports.EventSink that records everything published.
type Events struct {
	mu      sync.Mutex
	events  []ports.Event
	changed chan struct{}
}

// NewEvents returns an empty Events sink.
func NewEvents() *Events {
	return &Events{changed: make(chan struct{}, 1)}
}

// Publish records the event.
func (e *Events) Publish(ev ports.Event) {
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()

	select {
	case e.changed <- struct{}{}:
	default:
	}
}

// All returns a copy of every recorded event.
func (e *Events) All() []ports.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ports.Event(nil), e.events...)
}

// Count returns how many events of the given kind were recorded.
func (e *Events) Count(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// Wait blocks until at least n events of the given kind have been recorded, or
// the timeout expires. It reports whether the count was reached.
func (e *Events) Wait(kind string, n int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		if e.Count(kind) >= n {
			return true
		}
		select {
		case <-e.changed:
		case <-deadline.C:
			return e.Count(kind) >= n
		}
	}
}
