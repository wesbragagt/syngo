package tcpwire

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// fuzzDeviceID is a syntactically valid device id, used wherever a seed needs
// one.
var fuzzDeviceID = strings.Repeat("A", device.IDLength)

// FuzzManifestReply decodes an arbitrary ManifestResponse header and converts
// it to the domain manifest. The invariants are that every accepted entry
// names a path inside the folder, carries a size the receiver agreed to, and
// that converting the result back to the wire form and in again changes
// nothing.
func FuzzManifestReply(f *testing.F) {
	seeds := []string{
		// The entry shape of docs/PROTOCOL.md.
		`{"folder_id":"photos","sequence":7,"entries":[` +
			`{"path":"sub/a.txt","kind":1,"size":1234,"mtime_ns":1700000000123456789,` +
			`"mode":420,"hash":"` + strings.Repeat("ab", 32) + `","link_target":"","deleted":false,` +
			`"modified_by":"` + fuzzDeviceID + `","sequence":7,` +
			`"version":{"` + fuzzDeviceID + `":3}}]}`,
		`{"folder_id":"photos","sequence":0,"entries":[]}`,
		`{"folder_id":"photos","entries":[{"path":"dir","kind":2,"sequence":1}]}`,
		`{"folder_id":"photos","entries":[{"path":"l","kind":3,"link_target":"a.txt"}]}`,
		// Known-bad shapes.
		``,
		`{`,
		`null`,
		`[]`,
		`{"entries":[{"path":"../escape"}]}`,
		`{"entries":[{"path":"/abs"}]}`,
		`{"entries":[{"path":".syngo/x"}]}`,
		`{"entries":[{"path":".syngoignore"}]}`,
		`{"entries":[{"path":"a","size":-1}]}`,
		`{"entries":[{"path":"a","size":9223372036854775807}]}`,
		`{"entries":[{"path":"l","kind":3,"link_target":"/etc/passwd"}]}`,
		`{"entries":[{"path":"l","kind":3,"link_target":"../../out"}]}`,
		`{"entries":[{"path":"a","hash":"zz"}]}`,
		`{"entries":[{"path":"a","version":{"nope":1}}]}`,
		`{"entries":[{"path":"a","version":{"` + fuzzDeviceID + `":0}}]}`,
		`{"entries":[{"path":"` + strings.Repeat("a", 5000) + `"}]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, header []byte) {
		var h manifestReplyHeader
		if json.Unmarshal(header, &h) != nil {
			return
		}
		m, err := fromWireManifest(h, DefaultMaxFileSize)
		if err != nil {
			return
		}

		for _, e := range m.Sorted() {
			if err := manifest.ValidRelPath(e.Path); err != nil {
				t.Fatalf("accepted manifest holds the invalid path %q: %v", e.Path, err)
			}
			if e.Size < 0 || e.Size > DefaultMaxFileSize {
				t.Fatalf("accepted entry %q has the size %d", e.Path, e.Size)
			}
			if e.Kind == manifest.KindSymlink && !e.Deleted {
				if err := manifest.ValidLinkTarget(e.Path, e.LinkTarget); err != nil {
					t.Fatalf("accepted symlink %q targets %q: %v", e.Path, e.LinkTarget, err)
				}
			}
			// The version vector survives the wire form and the
			// text form the store writes.
			back, err := manifest.ParseVersion(e.Version.String())
			if err != nil {
				t.Fatalf("entry %q has the unparseable version %q: %v", e.Path, e.Version.String(), err)
			}
			if back.Compare(e.Version) != manifest.Equal {
				t.Fatalf("entry %q version %q does not round-trip", e.Path, e.Version.String())
			}
		}

		// Re-encoding an accepted manifest yields one that is accepted
		// again and holds the same entries.
		again, err := fromWireManifest(toWireManifest(m), DefaultMaxFileSize)
		if err != nil {
			t.Fatalf("re-encoding an accepted manifest made it invalid: %v", err)
		}
		if len(again.Entries) != len(m.Entries) {
			t.Fatalf("re-encoding changed the entry count: %d, want %d", len(again.Entries), len(m.Entries))
		}
		for i, e := range again.Sorted() {
			if got, want := e.Path, m.Sorted()[i].Path; got != want {
				t.Fatalf("re-encoding changed entry %d: %q, want %q", i, got, want)
			}
		}
	})
}

// FuzzChunkRequest decodes an arbitrary ChunkRequest header. An accepted
// request carries a hash that round-trips through its text form.
func FuzzChunkRequest(f *testing.F) {
	seeds := []string{
		`{"folder_id":"photos","path":"a.txt","hash":"` + strings.Repeat("ab", 32) + `","index":0,"offset":0,"length":262144}`,
		`{"folder_id":"photos","path":"a.txt","hash":"","index":0,"offset":0,"length":0}`,
		// Known-bad shapes.
		``,
		`{`,
		`{"hash":"zz"}`,
		`{"hash":"` + strings.Repeat("ab", 31) + `"}`,
		`{"path":"../escape","hash":""}`,
		`{"index":-1,"offset":-1,"length":-1,"hash":""}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, header []byte) {
		var h chunkRequestHeader
		if json.Unmarshal(header, &h) != nil {
			return
		}
		req, err := fromWireChunkRequest(h)
		if err != nil {
			return
		}
		// The hash is normalized to lowercase hex, and the all-zero
		// hash to the empty string, which is how a directory, a symlink
		// and a tombstone are stored. Past that normalization the text
		// form parses back to the same hash.
		text := req.Hash.String()
		normalized := strings.ToLower(h.Hash)
		if req.Hash.IsZero() {
			normalized = ""
		}
		if text != normalized {
			t.Fatalf("chunk request hash %q decoded to %q", h.Hash, text)
		}
		back, err := manifest.ParseHash(text)
		if err != nil {
			t.Fatalf("accepted chunk request holds an unparseable hash %q: %v", text, err)
		}
		if back != req.Hash {
			t.Fatalf("chunk request hash %q does not round-trip through %q", h.Hash, text)
		}
		if got := toWireChunkRequest(req); got.Path != h.Path || got.Hash != text ||
			got.Index != h.Index || got.Offset != h.Offset || got.Length != h.Length {
			t.Fatalf("chunk request did not round-trip: %+v, want %+v", got, h)
		}
	})
}

// FuzzHelloHeader decodes an arbitrary Hello header. An accepted Hello names a
// device id that parses, and re-encoding it changes neither the id nor the
// folder list.
func FuzzHelloHeader(f *testing.F) {
	seeds := []string{
		`{"protocol_version":4,"device_id":"` + fuzzDeviceID + `","device_name":"laptop",` +
			`"folders":[{"id":"photos","mode":"sendreceive"}]}`,
		`{"protocol_version":4,"device_id":"` + fuzzDeviceID + `","device_name":"","folders":null}`,
		// Known-bad shapes.
		``,
		`{}`,
		`{"protocol_version":1,"device_id":"` + fuzzDeviceID + `"}`,
		`{"device_id":"nope"}`,
		`{"device_id":"` + strings.Repeat("1", device.IDLength) + `"}`,
		`{"device_id":"` + fuzzDeviceID + `","folders":[{"id":"../x","mode":"nope"}]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, header []byte) {
		var h helloHeader
		if json.Unmarshal(header, &h) != nil {
			return
		}
		hello, err := fromWireHello(h)
		if err != nil {
			return
		}
		if _, err := device.ParseID(string(hello.DeviceID)); err != nil {
			t.Fatalf("accepted hello holds the invalid device id %q: %v", hello.DeviceID, err)
		}
		if len(hello.Folders) != len(h.Folders) {
			t.Fatalf("hello lost folders: %d, want %d", len(hello.Folders), len(h.Folders))
		}
		wire := toWireHello(hello)
		if wire.ProtocolVersion != ProtocolVersion {
			t.Fatalf("re-encoded hello announces version %d, want %d", wire.ProtocolVersion, ProtocolVersion)
		}
		for i, adv := range wire.Folders {
			if adv != h.Folders[i] {
				t.Fatalf("folder %d did not round-trip: %+v, want %+v", i, adv, h.Folders[i])
			}
		}
	})
}
