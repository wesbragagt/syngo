// Package ports declares the interfaces the application layer needs from the
// outside world. Adapters implement them; the composition root wires them up.
package ports

import "time"

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}
