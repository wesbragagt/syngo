package network_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/network"
	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/domain/backoff"
	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// testRebinder is the configuration the tests share: a hold check and a
// backoff short enough that a rebind happens within a test, and a clock the
// test moves itself so the reported times are its own.
func testRebinder(clock *fakes.Clock) network.RebinderConfig {
	return network.RebinderConfig{
		HoldCheck: time.Millisecond,
		Backoff:   backoff.Policy{Min: time.Millisecond, Max: 5 * time.Millisecond},
		Interface: "tailscale0",
		Clock:     clock,
	}
}

// serve runs the listener until the returned function is called.
func serve(t *testing.T, r *network.Rebinder) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, nil, nil) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after the context was cancelled")
		}
	}
}

// waitFor polls until want reports true, or fails the test.
func waitFor(t *testing.T, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRebindsAfterTheAddressIsTakenAway(t *testing.T) {
	clock := fakes.NewClock(time.Time{})
	binder := fakes.NewBinder("100.64.0.2:22000")
	r, err := network.Bind(binder, testRebinder(clock))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	stop := serve(t, r)
	defer stop()

	if state := r.State(); !state.Bound || state.Address != "100.64.0.2:22000" {
		t.Fatalf("after the first bind the state is %+v, want bound on 100.64.0.2:22000", state)
	}
	boundAt := r.State().BoundAt

	clock.Advance(time.Minute)
	binder.TakeAddress()
	waitFor(t, "the listener to notice the address is gone", func() bool {
		state := r.State()
		return !state.Bound && state.Failures > 0
	})

	lost := r.State()
	if lost.LostAt.IsZero() {
		t.Error("the state records no time for losing the address")
	}
	if lost.Error == "" {
		t.Error("the state says nothing about why the bind failed")
	}

	// A peer that cannot be reached must not cost the machine: the
	// attempts are spaced by the backoff, not by the loop.
	attempts := r.State().Failures
	time.Sleep(20 * time.Millisecond)
	if grew := r.State().Failures - attempts; grew > 20 {
		t.Errorf("the rebind loop made %d attempts in 20ms, which is spinning", grew)
	}

	clock.Advance(time.Minute)
	binder.ReturnAddress("")
	waitFor(t, "the listener to rebind", func() bool { return r.State().Bound })

	back := r.State()
	switch {
	case back.Address != "100.64.0.2:22000":
		t.Errorf("rebound on %q, want the address peers know", back.Address)
	case back.Rebinds != 1:
		t.Errorf("rebinds = %d, want 1", back.Rebinds)
	case back.Failures != 0:
		t.Errorf("failures = %d after a successful rebind, want 0", back.Failures)
	case !back.BoundAt.After(boundAt):
		t.Errorf("bound_at is %v, want a time after the first bind at %v", back.BoundAt, boundAt)
	case back.LostAt != lost.LostAt:
		t.Errorf("lost_at is %v, want it kept at %v so status can say what happened", back.LostAt, lost.LostAt)
	case back.Interface != "tailscale0":
		t.Errorf("interface = %q, want the one the address is taken from", back.Interface)
	}
	if binder.Binds() < 2 {
		t.Errorf("the binder handed out %d sockets, want a second one after the address came back", binder.Binds())
	}
}

func TestRebindsOnTheNewAddressAfterARenumber(t *testing.T) {
	binder := fakes.NewBinder("100.64.0.2:22000")
	r, err := network.Bind(binder, testRebinder(fakes.NewClock(time.Time{})))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	stop := serve(t, r)
	defer stop()

	binder.TakeAddress()
	waitFor(t, "the listener to lose its address", func() bool { return !r.State().Bound })
	binder.ReturnAddress("100.64.0.9:22000")
	waitFor(t, "the listener to rebind", func() bool { return r.State().Bound })

	if got := r.Addr(); got != "100.64.0.9:22000" {
		t.Errorf("Addr() = %q, want the address the interface came back with", got)
	}
}

func TestBroadcastsGoToTheCurrentSocketOnly(t *testing.T) {
	binder := fakes.NewBinder("100.64.0.2:22000")
	r, err := network.Bind(binder, testRebinder(fakes.NewClock(time.Time{})))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	stop := serve(t, r)
	defer stop()

	binder.TakeAddress()
	waitFor(t, "the listener to lose its address", func() bool { return !r.State().Bound })

	// Nothing is connected while the listener is rebinding, so a broadcast
	// is dropped rather than queued or panicking.
	r.Broadcast(folder.ID("docs"), 1)

	binder.ReturnAddress("")
	waitFor(t, "the listener to rebind", func() bool { return r.State().Bound })
	r.Broadcast(folder.ID("docs"), 2)

	listeners := binder.Listeners()
	if len(listeners) < 2 {
		t.Fatalf("the binder handed out %d sockets, want at least 2", len(listeners))
	}
	if got := len(listeners[len(listeners)-1].Changes()); got != 1 {
		t.Errorf("the current socket got %d broadcasts, want the one sent after the rebind", got)
	}
	if got := len(listeners[0].Changes()); got != 0 {
		t.Errorf("the closed socket got %d broadcasts, want none", got)
	}
}

func TestBindReportsTheFirstFailure(t *testing.T) {
	binder := fakes.NewBinder("100.64.0.2:22000")
	binder.TakeAddress()
	if _, err := network.Bind(binder, testRebinder(fakes.NewClock(time.Time{}))); err == nil {
		t.Fatal("Bind succeeded with no address to bind, want the failure reported so the daemon does not start")
	}
}

// instantListener is a bound listener whose Serve returns as soon as it is
// called, the way a socket that dies the moment it is bound would.
type instantListener struct {
	addr  string
	calls *atomic.Int64
}

// Serve counts the call and returns at once.
func (l *instantListener) Serve(context.Context, ports.PeerHandler, ports.DeviceLookup) error {
	l.calls.Add(1)
	return nil
}

// Addr returns the address the listener was bound at.
func (l *instantListener) Addr() string { return l.addr }

// Close does nothing: there is no socket behind this.
func (l *instantListener) Close() error { return nil }

// Broadcast, AnnounceFolders and RetainDevices are what ports.Listener asks
// for and this listener has nobody to tell.
func (l *instantListener) Broadcast(folder.ID, uint64)          {}
func (l *instantListener) AnnounceFolders([]ports.FolderAdvert) {}
func (l *instantListener) RetainDevices([]device.ID)            {}

// instantBinder always binds, and every socket it hands out stops serving
// immediately. It is the shape that makes an unguarded rebind loop spin: every
// bind succeeds, so nothing on the bind side ever backs off.
type instantBinder struct {
	calls atomic.Int64
	binds atomic.Int64
}

// Resolve always answers.
func (b *instantBinder) Resolve() (string, error) { return "100.64.0.2:22000", nil }

// Bind always succeeds.
func (b *instantBinder) Bind(addr string) (ports.BoundListener, error) {
	b.binds.Add(1)
	return &instantListener{addr: addr, calls: &b.calls}, nil
}

// Held always holds: nothing takes this address away.
func (b *instantBinder) Held(string) bool { return true }

func TestServeDoesNotSpinWhenTheSocketDiesAtOnce(t *testing.T) {
	binder := &instantBinder{}
	r, err := network.Bind(binder, network.RebinderConfig{
		HoldCheck: time.Millisecond,
		Settle:    50 * time.Millisecond,
		Backoff:   backoff.Policy{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond},
		Clock:     fakes.NewClock(time.Time{}),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, nil, nil) }()

	const window = 500 * time.Millisecond
	time.Sleep(window)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}

	// With a floor of 10ms and a ceiling of 50ms, 500ms of retrying is a
	// few dozen cycles at most. Anything near the thousands is the loop
	// running as fast as the machine will let it.
	const bound = 60
	if got := binder.calls.Load(); got > bound {
		t.Errorf("the socket was served %d times in %v, want at most %d: the loop is not spaced by the backoff",
			got, window, bound)
	}
	if binder.calls.Load() < 2 {
		t.Errorf("the socket was served %d times, want the listener to keep trying", binder.calls.Load())
	}
}

// blockingBinder always binds, and every socket it hands out serves until the
// test tells it to stop. It is how the healthy case is exercised: the address
// is never gone, so nothing on the bind side backs off and what is measured is
// the serve-ended edge alone.
type blockingBinder struct {
	mu     sync.Mutex
	served []chan struct{}
	starts atomic.Int64
}

// Resolve always answers.
func (b *blockingBinder) Resolve() (string, error) { return "100.64.0.2:22000", nil }

// Bind hands out a socket that serves until stopCurrent is called.
func (b *blockingBinder) Bind(addr string) (ports.BoundListener, error) {
	stop := make(chan struct{})
	b.mu.Lock()
	b.served = append(b.served, stop)
	b.mu.Unlock()
	return &blockingListener{addr: addr, stop: stop, starts: &b.starts}, nil
}

// Held always holds.
func (b *blockingBinder) Held(string) bool { return true }

// stopCurrent ends the socket that is serving now.
func (b *blockingBinder) stopCurrent() {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.served[len(b.served)-1])
}

// blockingListener serves until its stop channel is closed.
type blockingListener struct {
	addr   string
	stop   chan struct{}
	starts *atomic.Int64
}

// Serve blocks until the test stops it or the context ends.
func (l *blockingListener) Serve(ctx context.Context, _ ports.PeerHandler, _ ports.DeviceLookup) error {
	l.starts.Add(1)
	select {
	case <-ctx.Done():
	case <-l.stop:
	}
	return nil
}

// Addr returns the address the listener was bound at.
func (l *blockingListener) Addr() string { return l.addr }

// Close does nothing: there is no socket behind this.
func (l *blockingListener) Close() error { return nil }

// Broadcast, AnnounceFolders and RetainDevices have nobody to tell.
func (l *blockingListener) Broadcast(folder.ID, uint64)          {}
func (l *blockingListener) AnnounceFolders([]ports.FolderAdvert) {}
func (l *blockingListener) RetainDevices([]device.ID)            {}

func TestAHealthySocketIsReboundAtOnce(t *testing.T) {
	binder := &blockingBinder{}
	r, err := network.Bind(binder, network.RebinderConfig{
		HoldCheck: time.Millisecond,
		Settle:    20 * time.Millisecond,
		// A floor high enough that waiting it out is unmistakable.
		Backoff: backoff.Policy{Min: 2 * time.Second, Max: 5 * time.Second},
		Clock:   fakes.NewClock(time.Time{}),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	stop := serve(t, r)
	defer stop()

	waitFor(t, "the first socket to be serving", func() bool { return binder.starts.Load() == 1 })
	// It serves happily for longer than the settle time, so whatever
	// happens to it next is a new event and not a socket that cannot stay
	// up.
	time.Sleep(100 * time.Millisecond)
	binder.stopCurrent()

	deadline := time.Now().Add(time.Second)
	for binder.starts.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("a listener that had been serving happily waited out the backoff before binding again")
		}
		time.Sleep(time.Millisecond)
	}
}
