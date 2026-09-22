package network_test

import (
	"context"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/fakes"
	"github.com/wesbragagt/syncthis/internal/app/network"
	"github.com/wesbragagt/syncthis/internal/domain/backoff"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
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
