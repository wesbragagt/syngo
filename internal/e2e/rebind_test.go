package e2e

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/status"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// flappingBinder is a real TCP binder whose address the test can take away and
// give back. Taking it away is what the daemon sees when the Tailscale client
// restarts under it, when the machine wakes from sleep, or when the link
// carrying the address drops: the socket is closed, binding fails, and the
// address the daemon was listening on is no longer one this machine has.
type flappingBinder struct {
	inner ports.Binder

	mu   sync.Mutex
	gone bool
}

// newFlappingBinder wraps a binder so the test can take its address away.
func newFlappingBinder(inner ports.Binder) *flappingBinder {
	return &flappingBinder{inner: inner}
}

// Resolve refuses while the address is gone.
func (b *flappingBinder) Resolve() (string, error) {
	if b.down() {
		return "", os.ErrNotExist
	}
	return b.inner.Resolve()
}

// Bind refuses while the address is gone.
func (b *flappingBinder) Bind(addr string) (ports.BoundListener, error) {
	if b.down() {
		return nil, os.ErrNotExist
	}
	return b.inner.Bind(addr)
}

// Held reports that the machine has the address, unless the test took it.
func (b *flappingBinder) Held(addr string) bool {
	return !b.down() && b.inner.Held(addr)
}

// TakeAddress removes the address from the machine, as far as the daemon can
// tell.
func (b *flappingBinder) TakeAddress() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gone = true
}

// ReturnAddress gives it back.
func (b *flappingBinder) ReturnAddress() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gone = false
}

// down reports whether the address is gone.
func (b *flappingBinder) down() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gone
}

// waitListener polls a node's status until the listener section satisfies
// want, and returns it.
func waitListener(t *testing.T, n *node, what string, want func(status.Listener) bool) status.Listener {
	t.Helper()
	deadline := time.Now().Add(daemonTimeout)
	var last status.Listener
	for time.Now().Before(deadline) {
		last = n.Status().Daemon.Listener
		if want(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s on %s, listener is %+v", what, n.name, last)
	return last
}

// TestListenerRebindsAfterLosingItsAddress takes the address the daemon
// listens on away while it runs, gives it back, and checks that the daemon
// comes back by itself: nothing restarts it, nothing reconfigures it, and the
// folders converge again afterwards.
func TestListenerRebindsAfterLosingItsAddress(t *testing.T) {
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, RecoverableListener: true})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive})
	pair(t, a, b)

	writeFile(t, a, "before.txt", []byte("written before the address went away"))
	waitTreeEqual(t, a, b)

	bound := waitListener(t, a, "the listener to be bound", func(l status.Listener) bool { return l.Bound })
	if bound.BoundAt == nil {
		t.Fatal("the listener reports no bind time")
	}
	address := bound.Address

	// The address goes away under the running daemon.
	a.binder.TakeAddress()
	lost := waitListener(t, a, "the listener to notice the address is gone",
		func(l status.Listener) bool { return !l.Bound && l.Failures > 0 })
	switch {
	case lost.LostAt == nil:
		t.Error("the listener reports no time for losing its address")
	case lost.Error == nil:
		t.Error("the listener says nothing about why it cannot bind")
	case lost.Address != address:
		t.Errorf("the listener reports address %q while rebinding, want the one peers know, %q",
			lost.Address, address)
	}

	// While it is down, a change on the other side cannot arrive.
	writeFile(t, b, "during.txt", []byte("written while the address was gone"))

	// The address comes back, and so does the daemon, on the same port.
	a.binder.ReturnAddress()
	back := waitListener(t, a, "the listener to rebind", func(l status.Listener) bool { return l.Bound })
	switch {
	case back.Address != address:
		t.Errorf("rebound on %q, want the address peers are configured with, %q", back.Address, address)
	case back.Rebinds < 1:
		t.Errorf("rebinds = %d, want at least one", back.Rebinds)
	case back.LostAt == nil:
		t.Error("the listener forgot that it lost its address, so status cannot say what happened")
	case back.Failures != 0:
		t.Errorf("failures = %d after a successful rebind, want 0", back.Failures)
	}

	// What was written on both sides while the port was down converges
	// without anything being restarted.
	writeFile(t, a, "after.txt", []byte("written after the address came back"))
	waitTreeEqual(t, a, b)
	waitTreeEqual(t, b, a)
}
