package fakes

import (
	"errors"
	"sync"

	"github.com/wesbragagt/syngo/internal/app/ports"
)

var _ ports.Binder = (*Binder)(nil)

// ErrAddressGone is what a Binder whose address was taken away reports. It
// stands for what a machine says when the interface carrying the address has
// gone: a Tailscale client restarting, a link that dropped, a sleep.
var ErrAddressGone = errors.New("fake: the machine does not have this address")

// Binder is an in-memory ports.Binder. A test takes the address away and gives
// it back without touching the machine, which is how the rebinding logic is
// exercised without a network.
type Binder struct {
	mu sync.Mutex
	// addr is what Resolve returns while the address is held.
	addr string
	// gone reports that the machine no longer has the address.
	gone bool
	// binds counts the sockets handed out, so a test can prove that a
	// retry loop is bounded rather than spinning.
	binds int
	// bound is every listener Bind returned, newest last.
	bound []*Listener
}

// NewBinder returns a binder holding addr.
func NewBinder(addr string) *Binder { return &Binder{addr: addr} }

// Resolve returns the held address, or fails while it is gone.
func (b *Binder) Resolve() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gone {
		return "", ErrAddressGone
	}
	return b.addr, nil
}

// Bind returns a listener for addr, or fails while the address is gone.
func (b *Binder) Bind(addr string) (ports.BoundListener, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gone {
		return nil, ErrAddressGone
	}
	b.binds++
	ln := NewListenerAt(addr)
	b.bound = append(b.bound, ln)
	return ln, nil
}

// Held reports whether the machine still has the address.
func (b *Binder) Held(string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.gone
}

// TakeAddress makes the machine lose the address: Resolve and Bind fail and
// Held is false until ReturnAddress is called.
func (b *Binder) TakeAddress() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gone = true
}

// ReturnAddress gives the address back, optionally a different one, which is
// what a renumbered interface looks like. An empty address keeps the old one.
func (b *Binder) ReturnAddress(addr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if addr != "" {
		b.addr = addr
	}
	b.gone = false
}

// Binds reports how many sockets were handed out.
func (b *Binder) Binds() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.binds
}

// Listeners returns every listener Bind handed out, newest last.
func (b *Binder) Listeners() []*Listener {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*Listener(nil), b.bound...)
}
