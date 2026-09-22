package tcpwire

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

var _ ports.Binder = (*Binder)(nil)

// Binder binds the port peers connect to. It is the only thing that knows how
// a configured listen address and interface name become a socket, which is
// what lets the application rebind after the address goes away without
// knowing anything about interfaces.
type Binder struct {
	cert tls.Certificate
	log  *slog.Logger
	// listen, iface and allowPublic are the configuration a bind starts
	// from, exactly as `daemon run` was given them.
	listen      string
	iface       string
	allowPublic bool

	mu sync.Mutex
	// port is the port the first bind ended up on. Every later bind asks
	// for it again, so a listener that comes back is reachable at the
	// address its peers already have, even when the first bind asked for
	// port 0.
	port string
}

// NewBinder returns a binder for a listen address and an optional interface
// name. Nothing is resolved or bound until Resolve and Bind are called.
func NewBinder(cert tls.Certificate, listen, iface string, allowPublic bool, log *slog.Logger) *Binder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Binder{cert: cert, log: log, listen: listen, iface: iface, allowPublic: allowPublic}
}

// Resolve returns the address to bind now. An interface name is turned into
// its current first IPv4 address on every call, so a device that came back
// with another address binds the new one, and an interface that is not there
// yet is an error the caller retries.
func (b *Binder) Resolve() (string, error) {
	listen := b.listen
	if port := b.boundPort(); port != "" {
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			return "", fmt.Errorf("%w: %q must be host:port: %v", ErrListenAddress, listen, err)
		}
		listen = net.JoinHostPort(host, port)
	}

	return ResolveListen(listen, b.iface, b.allowPublic)
}

// Bind binds addr and returns a listener for it. A machine that does not have
// the address fails with EADDRNOTAVAIL, which is what a Tailscale client
// restarting under a running daemon looks like; the caller waits and asks
// again.
func (b *Binder) Bind(addr string) (ports.BoundListener, error) {
	ln, err := Listen(b.cert, addr, b.log)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	if _, port, splitErr := net.SplitHostPort(ln.Addr()); splitErr == nil {
		b.port = port
	}
	b.mu.Unlock()
	return ln, nil
}

// Held reports whether this machine still has the address. A wildcard or
// loopback address is always held; anything else has to be configured on an
// interface right now.
func (b *Binder) Held(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if host == "" || ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
		return true
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		// Nothing was learned, so nothing is claimed: a listener is
		// not torn down because the machine could not be asked.
		return true
	}
	for _, each := range addrs {
		ipNet, ok := each.(*net.IPNet)
		if ok && ipNet.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// boundPort returns the port the first bind ended up on, or "".
func (b *Binder) boundPort() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.port
}
