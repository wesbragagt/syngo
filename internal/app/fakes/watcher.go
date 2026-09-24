package fakes

import (
	"context"
	"sync"

	"github.com/wesbragagt/syngo/internal/app/ports"
)

// Watcher is a ports.Watcher whose events are pushed by the test.
type Watcher struct {
	mu    sync.Mutex
	roots []string
	out   chan<- ports.WatchEvent
	ready chan struct{}
}

// NewWatcher returns a Watcher that is not yet watching.
func NewWatcher() *Watcher {
	return &Watcher{ready: make(chan struct{})}
}

// Watch records the root and forwards pushed events until ctx is done.
func (w *Watcher) Watch(ctx context.Context, root string, out chan<- ports.WatchEvent) error {
	w.mu.Lock()
	w.roots = append(w.roots, root)
	first := w.out == nil
	w.out = out
	w.mu.Unlock()
	if first {
		close(w.ready)
	}

	<-ctx.Done()
	return ctx.Err()
}

// Roots returns the roots Watch was called with.
func (w *Watcher) Roots() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.roots...)
}

// WaitReady blocks until Watch has been called at least once.
func (w *Watcher) WaitReady() { <-w.ready }

// Push delivers an event to the watcher's output channel. It blocks until the
// event is consumed or ctx is done.
func (w *Watcher) Push(ctx context.Context, e ports.WatchEvent) error {
	w.WaitReady()
	w.mu.Lock()
	out := w.out
	w.mu.Unlock()

	select {
	case out <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
