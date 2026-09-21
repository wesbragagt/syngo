package tcpwire

import "time"

// SetTimeouts shortens a dialer's timings so tests do not wait minutes for an
// idle connection to be noticed.
func (d *Dialer) SetTimeouts(ping, idle, dial time.Duration) {
	d.pingInterval, d.idleTimeout, d.dialTimeout = ping, idle, dial
}

// SetTimeouts shortens a listener's timings for tests.
func (l *Listener) SetTimeouts(handshake, idle time.Duration) {
	l.handshakeTimeout, l.idleTimeout = handshake, idle
}
