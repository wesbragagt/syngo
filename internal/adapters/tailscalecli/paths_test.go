package tailscalecli_test

import (
	"os"
	"testing"

	"github.com/wesbragagt/syngo/internal/adapters/tailscalecli"
	"github.com/wesbragagt/syngo/internal/app/ports"
)

// readStatus reads one recorded `tailscale status --json` document.
func readStatus(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

// pathOf returns the path of the node with the given host name.
func pathOf(t *testing.T, paths []ports.TailnetPath, host string) ports.TailnetPath {
	t.Helper()
	for _, path := range paths {
		if path.Node.HostName == host {
			return path
		}
	}
	t.Fatalf("no path for %q in %+v", host, paths)
	return ports.TailnetPath{}
}

func TestParsePathsReadsDirectAndRelay(t *testing.T) {
	paths, err := tailscalecli.ParsePaths(readStatus(t, "status_old.json"))
	if err != nil {
		t.Fatalf("ParsePaths error: %v", err)
	}

	nas := pathOf(t, paths, "nas")
	switch {
	case nas.Kind != ports.TailnetPathDirect:
		t.Errorf("nas kind = %q, want direct: the client names a current endpoint", nas.Kind)
	case nas.Endpoint != "198.51.100.9:41641":
		t.Errorf("nas endpoint = %q, want the endpoint the client named", nas.Endpoint)
	case nas.Relay != "":
		t.Errorf("nas relay = %q, want none on a direct path", nas.Relay)
	case !nas.Active:
		t.Error("nas is reported as inactive, but the client says traffic is flowing")
	}

	// A node with no endpoint and no relay is never guessed either way.
	phone := pathOf(t, paths, "phone")
	if phone.Kind != ports.TailnetPathUnknown || phone.Reason == "" {
		t.Errorf("phone path = %+v, want unknown with a reason", phone)
	}
}

func TestParsePathsReadsTheRelayRegion(t *testing.T) {
	paths, err := tailscalecli.ParsePaths(readStatus(t, "status_current.json"))
	if err != nil {
		t.Fatalf("ParsePaths error: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("got %d paths, want one per peer", len(paths))
	}
	if got := paths[0].Node.HostName; got != "desktop" {
		t.Errorf("first path is for %q, want the peers sorted by host name", got)
	}

	nas := pathOf(t, paths, "nas")
	if nas.Kind != ports.TailnetPathRelay || nas.Relay != "iad" {
		t.Errorf("nas path = %+v, want relay through iad", nas)
	}
	if nas.Endpoint != "" {
		t.Errorf("nas endpoint = %q, want none on a relayed path", nas.Endpoint)
	}
}

func TestParsePathsRejectsWhatItCannotRead(t *testing.T) {
	if _, err := tailscalecli.ParsePaths([]byte("not json")); err == nil {
		t.Fatal("ParsePaths accepted a document that is not JSON")
	}
}
