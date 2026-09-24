package pipeline_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/pipeline"
)

func TestBudget_HoldsWhatIsInFlightUnderTheLimit(t *testing.T) {
	b := pipeline.NewBudget(100)
	ctx := context.Background()

	for range 10 {
		if err := b.Acquire(ctx, 10); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}
	if got := b.InFlight(); got != 100 {
		t.Fatalf("in flight = %d, want the budget of 100", got)
	}

	// The eleventh does not fit and waits.
	tight, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := b.Acquire(tight, 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire over the budget = %v, want the deadline", err)
	}

	b.Release(10)
	if err := b.Acquire(ctx, 10); err != nil {
		t.Fatalf("Acquire after a release: %v", err)
	}
	if got := b.InFlight(); got != 100 {
		t.Fatalf("in flight = %d, want 100", got)
	}
}

// TestBudget_NeverExceedsTheLimitUnderLoad is the property the memory bound
// rests on: whatever the interleaving, the bytes held at any moment are never
// more than the budget.
func TestBudget_NeverExceedsTheLimitUnderLoad(t *testing.T) {
	const limit = 1000
	b := pipeline.NewBudget(limit)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			size := 50 + i%200
			for range 20 {
				if err := b.Acquire(ctx, size); err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				if got := b.InFlight(); got > limit {
					t.Errorf("%d bytes in flight, the budget is %d", got, limit)
				}
				b.Release(size)
			}
		}(i)
	}
	wg.Wait()

	if got := b.InFlight(); got != 0 {
		t.Fatalf("%d bytes still held after every release", got)
	}
}

// TestBudget_AChunkLargerThanTheBudgetStillPasses proves a single oversized
// chunk is let through rather than deadlocking a transfer forever.
func TestBudget_AChunkLargerThanTheBudgetStillPasses(t *testing.T) {
	b := pipeline.NewBudget(10)
	if err := b.Acquire(context.Background(), 1000); err != nil {
		t.Fatalf("Acquire of more than the budget: %v", err)
	}
	b.Release(1000)
	if got := b.InFlight(); got != 0 {
		t.Fatalf("%d bytes still held", got)
	}
}

// TestBudget_ZeroIsUnlimited proves the pinned-off case costs nothing.
func TestBudget_ZeroIsUnlimited(t *testing.T) {
	var b *pipeline.Budget
	if err := b.Acquire(context.Background(), 1<<30); err != nil {
		t.Fatalf("Acquire on a nil budget: %v", err)
	}
	b.Release(1 << 30)
	if got := pipeline.NewBudget(0).Limit(); got != 0 {
		t.Fatalf("limit = %d, want unlimited", got)
	}
}

func TestNext_MovesWithWhatDominates(t *testing.T) {
	cases := []struct {
		name              string
		limit             int
		netTime, diskTime time.Duration
		want              int
	}{
		{"the wire dominates, so another request fills the wait", 4, 100, 10, 5},
		{"the disk dominates, so a request would only queue bytes", 4, 10, 100, 3},
		{"neither dominates, so nothing moves", 4, 150, 100, 4},
		{"exactly twice is not more than twice", 4, 200, 100, 4},
		{"the floor is sequential", 1, 10, 100, pipeline.Floor},
		{"the ceiling is the ceiling", pipeline.Ceiling, 1000, 1, pipeline.Ceiling},
		{"a limit below the floor is raised to it", -5, 150, 100, pipeline.Floor},
		{"a limit above the ceiling is lowered to it", 99, 150, 100, pipeline.Ceiling},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pipeline.Next(c.limit, c.netTime, c.diskTime); got != c.want {
				t.Fatalf("Next(%d, %v, %v) = %d, want %d", c.limit, c.netTime, c.diskTime, got, c.want)
			}
		})
	}
}

// TestLimiter_RisesOnANetworkBoundTransfer proves the controller reacts only
// once a window has been measured, and then in the right direction.
func TestLimiter_RisesOnANetworkBoundTransfer(t *testing.T) {
	l := pipeline.Adaptive()
	start := l.Limit()

	for range 15 {
		l.Observe(100*time.Millisecond, time.Millisecond)
	}
	if got := l.Limit(); got != start {
		t.Fatalf("limit = %d after 15 chunks, want it held at %d until a window is full", got, start)
	}
	l.Observe(100*time.Millisecond, time.Millisecond)
	if got := l.Limit(); got != start+1 {
		t.Fatalf("limit = %d, want %d after a network-bound window", got, start+1)
	}
}

func TestLimiter_FallsToTheFloorOnADiskBoundTransfer(t *testing.T) {
	l := pipeline.Adaptive()
	for range 16 * 20 {
		l.Observe(time.Millisecond, 100*time.Millisecond)
	}
	if got := l.Limit(); got != pipeline.Floor {
		t.Fatalf("limit = %d, want the floor of %d", got, pipeline.Floor)
	}
}

func TestLimiter_NeverLeavesItsBounds(t *testing.T) {
	l := pipeline.Adaptive()
	for range 16 * 50 {
		l.Observe(time.Second, time.Nanosecond)
		if got := l.Limit(); got < pipeline.Floor || got > pipeline.Ceiling {
			t.Fatalf("limit = %d, outside [%d,%d]", got, pipeline.Floor, pipeline.Ceiling)
		}
	}
	if got := l.Limit(); got != pipeline.Ceiling {
		t.Fatalf("limit = %d, want the ceiling of %d", got, pipeline.Ceiling)
	}
}

// TestLimiter_PinnedIgnoresEveryMeasurement is what a constrained device and a
// test rely on: a pinned pipeline stays where it was put.
func TestLimiter_PinnedIgnoresEveryMeasurement(t *testing.T) {
	l := pipeline.Pinned(1)
	for range 16 * 10 {
		l.Observe(time.Second, time.Nanosecond)
	}
	if got := l.Limit(); got != 1 {
		t.Fatalf("limit = %d, want the pin of 1", got)
	}
	if !l.Pinned() {
		t.Fatal("a pinned limiter does not report itself pinned")
	}
	if got := pipeline.Pinned(999).Limit(); got != pipeline.Ceiling {
		t.Fatalf("a pin of 999 = %d, want the ceiling of %d", got, pipeline.Ceiling)
	}
	if got := pipeline.Pinned(0).Limit(); got != pipeline.Floor {
		t.Fatalf("a pin of 0 = %d, want the floor of %d", got, pipeline.Floor)
	}
}

// TestLimiter_ConcurrentObserversSeeAValidLimit runs the controller the way
// the puller does, from many chunk workers at once.
func TestLimiter_ConcurrentObserversSeeAValidLimit(t *testing.T) {
	l := pipeline.Adaptive()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 100 {
				l.Observe(time.Duration(i)*time.Millisecond, time.Millisecond)
				if got := l.Limit(); got < pipeline.Floor || got > pipeline.Ceiling {
					t.Errorf("limit = %d, outside [%d,%d]", got, pipeline.Floor, pipeline.Ceiling)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
