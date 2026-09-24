package ports

import (
	"context"

	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
)

// Limiter paces how many bytes per second pass through it. One limiter stands
// for one direction of one device: every folder and every peer connection
// shares it.
type Limiter interface {
	// Wait blocks until n bytes may pass or ctx ends. n <= 0 returns at
	// once, and so does a limiter whose rate is unlimited.
	Wait(ctx context.Context, n int) error
	// SetRate changes the limit while waiters are pending. Unlimited wakes
	// every waiter at once.
	SetRate(r bandwidth.Rate)
	// Rate returns the limit in force.
	Rate() bandwidth.Rate
}
