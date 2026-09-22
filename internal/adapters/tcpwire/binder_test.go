package tcpwire_test

import (
	"net"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/adapters/tcpwire"
)

func TestBinderKeepsThePortItFirstBound(t *testing.T) {
	binder := tcpwire.NewBinder(newIdentity(t, "laptop").cert, "127.0.0.1:0", "", false, nil)

	addr, err := binder.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	first, err := binder.Bind(addr)
	if err != nil {
		t.Fatalf("bind %s: %v", addr, err)
	}
	bound := first.Addr()
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A listener that comes back has to be reachable at the address its
	// peers already have, not at whatever port zero picks next.
	again, err := binder.Resolve()
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if again != bound {
		t.Fatalf("second resolve = %q, want the address the first bind ended up on, %q", again, bound)
	}

	second, err := binder.Bind(again)
	if err != nil {
		t.Fatalf("rebind %s: %v", again, err)
	}
	defer func() { _ = second.Close() }()
	if second.Addr() != bound {
		t.Fatalf("rebound on %q, want %q", second.Addr(), bound)
	}
}

func TestBinderHeldFollowsTheMachine(t *testing.T) {
	binder := tcpwire.NewBinder(newIdentity(t, "laptop").cert, "127.0.0.1:0", "", false, nil)

	if !binder.Held("127.0.0.1:22000") {
		t.Error("loopback is reported as not held, but this machine always has it")
	}
	// A Tailscale address this machine does not have is exactly what the
	// daemon sees after the client is stopped under it.
	if binder.Held("100.64.213.213:22000") {
		t.Error("an address this machine does not have is reported as held")
	}
	if binder.Held("not-an-address") {
		t.Error("something that is not an address is reported as held")
	}
}

func TestBinderReportsWhyItCannotBind(t *testing.T) {
	binder := tcpwire.NewBinder(newIdentity(t, "laptop").cert, "100.64.213.213:22000", "", false, nil)

	addr, err := binder.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := binder.Bind(addr); err == nil {
		t.Fatal("bind succeeded on an address this machine does not have")
	} else if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error = %v, want it to say what failed", err)
	}
}

func TestBinderRefusesAPublicAddress(t *testing.T) {
	binder := tcpwire.NewBinder(newIdentity(t, "laptop").cert, "0.0.0.0:22000", "", false, nil)
	if _, err := binder.Resolve(); err == nil {
		t.Fatal("resolve accepted a wildcard bind without --allow-public")
	}

	allowed := tcpwire.NewBinder(newIdentity(t, "laptop").cert, "0.0.0.0:22000", "", true, nil)
	addr, err := allowed.Resolve()
	if err != nil {
		t.Fatalf("resolve with --allow-public: %v", err)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatalf("resolved %q, want a host:port address: %v", addr, err)
	}
}
