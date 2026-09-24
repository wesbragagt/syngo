package backoff_test

import (
	"testing"
	"time"

	"github.com/wesbragagt/syngo/internal/domain/backoff"
)

func TestWaitDoublesUpToMax(t *testing.T) {
	p := backoff.Policy{Min: time.Second, Max: 8 * time.Second}
	want := []time.Duration{
		time.Second, time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 8 * time.Second, 8 * time.Second,
	}
	for failures, expected := range want {
		if got := p.Wait(failures); got != expected {
			t.Errorf("Wait(%d) = %v, want %v", failures, got, expected)
		}
	}
}

func TestWaitNeverReturnsZero(t *testing.T) {
	var p backoff.Policy
	for _, failures := range []int{-5, 0, 1, 100, 1 << 20} {
		got := p.Wait(failures)
		if got < backoff.DefaultMin || got > backoff.DefaultMax {
			t.Errorf("Wait(%d) = %v, want between %v and %v",
				failures, got, backoff.DefaultMin, backoff.DefaultMax)
		}
	}
}

func TestWaitClampsMaxBelowMin(t *testing.T) {
	p := backoff.Policy{Min: 5 * time.Second, Max: time.Second}
	if got := p.Wait(10); got != 5*time.Second {
		t.Errorf("Wait(10) = %v, want the floor 5s", got)
	}
}
