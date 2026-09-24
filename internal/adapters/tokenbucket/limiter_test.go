package tokenbucket_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/adapters/tokenbucket"
	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
)

// fakeClock is a clock that only moves when a sleep asks it to, which makes
// every wait the limiter decides on exactly reproducible.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	total  time.Duration
	delays []time.Duration
	// onSleep runs before the clock is advanced, so a test can change the
	// limiter while a wait is pending.
	onSleep func(n int, d time.Duration)
}

// newFakeClock returns a clock at a fixed instant.
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC)}
}

// Now returns the current fake time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep records the delay and advances the clock by exactly that much.
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	hook := c.onSleep
	n := len(c.delays)
	c.delays = append(c.delays, d)
	c.total += d
	c.mu.Unlock()

	if hook != nil {
		hook(n, d)
	}

	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

// stats returns how long was slept in total and how often.
func (c *fakeClock) stats() (time.Duration, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total, len(c.delays)
}

// drain runs n waits of size bytes each and returns the total sleep.
func drain(t *testing.T, rate bandwidth.Rate, waits, size int) (*fakeClock, time.Duration) {
	t.Helper()
	clock := newFakeClock()
	limiter := tokenbucket.NewWithClock(rate, clock.Now, clock.Sleep)
	for i := 0; i < waits; i++ {
		if err := limiter.Wait(context.Background(), size); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	total, _ := clock.stats()
	return clock, total
}

// TestWait_20MiBAt4MiBPerSecondSleepsAboutFiveSeconds pins the claim the CLI
// documentation makes: 20 MiB at 4 MiB/s takes five seconds, less the burst
// the bucket starts with.
func TestWait_20MiBAt4MiBPerSecondSleepsAboutFiveSeconds(t *testing.T) {
	const rate = bandwidth.Rate(4 << 20)

	_, big := drain(t, rate, 20, 1<<20)
	if big < 4850*time.Millisecond || big > 4950*time.Millisecond {
		t.Fatalf("20 waits of 1 MiB slept %v, want about 4.9s", big)
	}

	_, small := drain(t, rate, 1280, 16<<10)
	if small < 4850*time.Millisecond || small > 4950*time.Millisecond {
		t.Fatalf("1280 waits of 16 KiB slept %v, want about 4.9s", small)
	}
	if diff := big - small; diff > time.Millisecond || diff < -time.Millisecond {
		t.Fatalf("chunk size changed the total by %v, want the same within 1ms", diff)
	}
}

func TestWait_UnlimitedNeverSleeps(t *testing.T) {
	clock := newFakeClock()
	limiter := tokenbucket.NewWithClock(bandwidth.Unlimited, clock.Now, clock.Sleep)
	for i := 0; i < 8; i++ {
		if err := limiter.Wait(context.Background(), 64<<20); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	if total, calls := clock.stats(); calls != 0 || total != 0 {
		t.Fatalf("unlimited slept %v in %d calls, want none", total, calls)
	}
}

func TestWait_NonPositiveNeverSleeps(t *testing.T) {
	clock := newFakeClock()
	limiter := tokenbucket.NewWithClock(bandwidth.Rate(1), clock.Now, clock.Sleep)
	if err := limiter.Wait(context.Background(), 0); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if _, calls := clock.stats(); calls != 0 {
		t.Fatalf("a wait for no bytes slept %d times, want none", calls)
	}
}

func TestSetRate_ToUnlimitedWakesAWaiter(t *testing.T) {
	limiter := tokenbucket.New(bandwidth.Rate(1))
	done := make(chan error, 1)
	go func() { done <- limiter.Wait(context.Background(), 1<<20) }()

	time.Sleep(20 * time.Millisecond)
	limiter.SetRate(bandwidth.Unlimited)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the waiter did not wake when the limit was lifted")
	}
	if got := limiter.Rate(); !got.Unlimited() {
		t.Fatalf("rate = %v, want unlimited", got)
	}
}

// TestSetRate_LowerRateStretchesAPendingWait lowers the rate from inside the
// first sleep, so the wait has to decide again: the second sleep is computed
// against the new rate, not the one the wait started under.
func TestSetRate_LowerRateStretchesAPendingWait(t *testing.T) {
	clock := newFakeClock()
	limiter := tokenbucket.NewWithClock(bandwidth.Rate(4<<20), clock.Now, clock.Sleep)
	clock.onSleep = func(n int, _ time.Duration) {
		if n == 0 {
			limiter.SetRate(bandwidth.Rate(1 << 20))
		}
	}

	if err := limiter.Wait(context.Background(), 4<<20); err != nil {
		t.Fatalf("wait: %v", err)
	}

	clock.mu.Lock()
	delays := append([]time.Duration(nil), clock.delays...)
	clock.mu.Unlock()
	if len(delays) != 2 {
		t.Fatalf("delays = %v, want two: one per rate", delays)
	}
	if delays[0] != 900*time.Millisecond {
		t.Fatalf("first delay = %v, want 900ms at 4 MiB/s", delays[0])
	}
	if delays[1] <= delays[0] {
		t.Fatalf("delays = %v, want the second one longer at the lower rate", delays)
	}
	if got := limiter.Rate(); got != bandwidth.Rate(1<<20) {
		t.Fatalf("rate = %v, want 1 MiB/s", got)
	}
}

func TestWait_ContextCancelledReturnsError(t *testing.T) {
	limiter := tokenbucket.New(bandwidth.Rate(1))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- limiter.Wait(ctx, 1<<20) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a cancelled wait did not return")
	}
}

// TestWait_RealClock_LowerBoundHolds is the only assertion made against the
// machine's own clock: scheduling can only ever lengthen a wait, so the limit
// holding is a lower bound and the upper bound is loose.
func TestWait_RealClock_LowerBoundHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	limiter := tokenbucket.New(bandwidth.Rate(4 << 20))

	started := time.Now()
	for i := 0; i < 8; i++ {
		if err := limiter.Wait(context.Background(), 256<<10); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	elapsed := time.Since(started)

	if elapsed < 400*time.Millisecond {
		t.Fatalf("2 MiB at 4 MiB/s took %v, want at least 400ms", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("2 MiB at 4 MiB/s took %v, want well under 5s", elapsed)
	}
}
