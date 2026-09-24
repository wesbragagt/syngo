// Package sysclock provides a ports.Clock backed by the system clock.
package sysclock

import (
	"time"

	"github.com/wesbragagt/syngo/internal/app/ports"
)

var _ ports.Clock = Clock{}

// Clock reads the wall clock.
type Clock struct{}

// New returns a system clock.
func New() Clock { return Clock{} }

// Now returns the current time.
func (Clock) Now() time.Time { return time.Now() }
