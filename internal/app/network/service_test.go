package network_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/app/fakes"
	"github.com/wesbragagt/syngo/internal/app/network"
	"github.com/wesbragagt/syngo/internal/app/ports"
)

func TestDetect_AbsentIsNotAnError(t *testing.T) {
	s := network.New(fakes.NewTailnetUnavailable())

	got, err := s.Detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Present || got.Running || got.BackendState != "" || got.IPs == nil || len(got.IPs) != 0 {
		t.Fatalf("detection = %+v, want an empty absent answer with an empty ip list", got)
	}
}

func TestDetect_NoTailnetConfiguredIsAbsent(t *testing.T) {
	got, err := network.New(nil).Detect(context.Background())
	if err != nil || got.Present {
		t.Fatalf("detect = %+v, %v, want absent and no error", got, err)
	}
}

func TestDetect_RunningCopiesSelf(t *testing.T) {
	s := network.New(fakes.NewTailnet())

	got, err := s.Detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !got.Present || !got.Running {
		t.Fatalf("detection = %+v, want a present, running client", got)
	}
	if got.HostName != "self" || got.DNSName != "self.tail1234.ts.net" ||
		got.Interface != "tailscale0" || got.MagicDNSSuffix != "tail1234.ts.net" ||
		len(got.IPs) != 1 || got.IPs[0] != "100.64.0.2" {
		t.Fatalf("detection = %+v, want it to copy self", got)
	}
}

func TestDetect_OtherErrorsPassThrough(t *testing.T) {
	tailnet := fakes.NewTailnet()
	boom := errors.New("boom")
	tailnet.SetError(boom)

	if _, err := network.New(tailnet).Detect(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}
}

func TestPeers_PassesUnavailableThrough(t *testing.T) {
	s := network.New(fakes.NewTailnetUnavailable())

	_, err := s.Peers(context.Background())
	if !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want ErrTailnetUnavailable", err)
	}

	if _, err := network.New(nil).Peers(context.Background()); !errors.Is(err, ports.ErrTailnetUnavailable) {
		t.Fatalf("error = %v, want ErrTailnetUnavailable without a tailnet", err)
	}
}

func TestPeers_SortedByHostName(t *testing.T) {
	s := network.New(fakes.NewTailnet(fakes.TailnetNode("nas", "100.64.0.3"), fakes.TailnetNode("desk", "100.64.0.4")))

	peers, err := s.Peers(context.Background())
	if err != nil {
		t.Fatalf("peers: %v", err)
	}
	if len(peers) != 2 || peers[0].HostName != "desk" || peers[1].HostName != "nas" {
		t.Fatalf("peers = %+v, want them sorted by host name", peers)
	}
}

func TestDetect_InstalledButUnusableIsPresentWithAReason(t *testing.T) {
	tailnet := fakes.NewTailnet()
	tailnet.SetError(fmt.Errorf("%w: tailscale status --json: exit status 1",
		ports.ErrTailnetUnavailable))

	got, err := network.New(tailnet).Detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !got.Present {
		t.Fatalf("detection = %+v, want the installed client reported as present", got)
	}
	if !strings.Contains(got.Err, "exit status 1") {
		t.Fatalf("error = %q, want the reason the client could not answer", got.Err)
	}
	if got.Running || got.Version != "" || len(got.IPs) != 0 {
		t.Fatalf("detection = %+v, want nothing but the reason known", got)
	}
}

func TestDetect_NotInstalledCarriesNoReason(t *testing.T) {
	got, err := network.New(fakes.NewTailnetUnavailable()).Detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if got.Present || got.Err != "" {
		t.Fatalf("detection = %+v, want an absent client with no reason", got)
	}
}
