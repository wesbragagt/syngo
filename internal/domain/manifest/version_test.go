package manifest_test

import (
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// Device ids of the tests, in ascending order so the expected String form of
// a vector is easy to read.
var (
	devA = device.ID(strings.Repeat("A", device.IDLength))
	devB = device.ID(strings.Repeat("B", device.IDLength))
	devC = device.ID(strings.Repeat("C", device.IDLength))
)

// version builds a vector from pairs, failing the test on invalid input.
func version(t *testing.T, counters ...manifest.Counter) manifest.Version {
	t.Helper()
	v, err := manifest.NewVersion(counters)
	if err != nil {
		t.Fatalf("NewVersion(%v) error: %v", counters, err)
	}
	return v
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		name string
		a, b manifest.Version
		want manifest.Ordering
	}{
		{"both empty", manifest.Version{}, manifest.Version{}, manifest.Equal},
		{"empty against a counter", manifest.Version{}, version(t, manifest.Counter{ID: devA, N: 1}), manifest.Before},
		{"a counter against empty", version(t, manifest.Counter{ID: devA, N: 1}), manifest.Version{}, manifest.After},
		{"same counter", version(t, manifest.Counter{ID: devA, N: 2}), version(t, manifest.Counter{ID: devA, N: 2}), manifest.Equal},
		{"lower counter", version(t, manifest.Counter{ID: devA, N: 1}), version(t, manifest.Counter{ID: devA, N: 2}), manifest.Before},
		{"higher counter", version(t, manifest.Counter{ID: devA, N: 2}), version(t, manifest.Counter{ID: devA, N: 1}), manifest.After},
		{"different devices", version(t, manifest.Counter{ID: devA, N: 1}), version(t, manifest.Counter{ID: devB, N: 1}), manifest.Concurrent},
		{
			"each ahead on one device",
			version(t, manifest.Counter{ID: devA, N: 2}, manifest.Counter{ID: devB, N: 1}),
			version(t, manifest.Counter{ID: devA, N: 1}, manifest.Counter{ID: devB, N: 2}),
			manifest.Concurrent,
		},
		{
			"extra device dominates",
			version(t, manifest.Counter{ID: devA, N: 2}, manifest.Counter{ID: devB, N: 1}),
			version(t, manifest.Counter{ID: devA, N: 2}),
			manifest.After,
		},
		{
			"merge of two dominates a third",
			version(t, manifest.Counter{ID: devA, N: 2}, manifest.Counter{ID: devB, N: 1}),
			version(t, manifest.Counter{ID: devA, N: 1}, manifest.Counter{ID: devC, N: 1}),
			manifest.Concurrent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Compare(tc.b); got != tc.want {
				t.Fatalf("Compare = %s, want %s", got, tc.want)
			}
			if got, want := tc.b.Compare(tc.a), mirror(tc.want); got != want {
				t.Fatalf("mirrored Compare = %s, want %s", got, want)
			}
		})
	}
}

// mirror is the ordering the reversed comparison must report.
func mirror(o manifest.Ordering) manifest.Ordering {
	switch o {
	case manifest.Before:
		return manifest.After
	case manifest.After:
		return manifest.Before
	default:
		return o
	}
}

func TestVersionCompareIsAntisymmetric(t *testing.T) {
	var all []manifest.Version
	for _, a := range []uint64{0, 1, 2} {
		for _, b := range []uint64{0, 1, 2} {
			var counters []manifest.Counter
			if a > 0 {
				counters = append(counters, manifest.Counter{ID: devA, N: a})
			}
			if b > 0 {
				counters = append(counters, manifest.Counter{ID: devB, N: b})
			}
			all = append(all, version(t, counters...))
		}
	}
	for _, x := range all {
		for _, y := range all {
			if got, want := y.Compare(x), mirror(x.Compare(y)); got != want {
				t.Fatalf("%q vs %q: %s, want %s", x, y, got, want)
			}
		}
	}
}

func TestVersionMerge(t *testing.T) {
	a := version(t, manifest.Counter{ID: devA, N: 2}, manifest.Counter{ID: devB, N: 1})
	b := version(t, manifest.Counter{ID: devA, N: 1}, manifest.Counter{ID: devC, N: 3})

	merged := a.Merge(b)
	if merged.Compare(a) != manifest.After || merged.Compare(b) != manifest.After {
		t.Fatalf("merge %q does not dominate %q and %q", merged, a, b)
	}
	if got := b.Merge(a); !got.Equals(merged) {
		t.Fatalf("merge is not commutative: %q vs %q", got, merged)
	}
	if got := merged.Merge(merged); !got.Equals(merged) {
		t.Fatalf("merge is not idempotent: %q", got)
	}
	if got := a.Merge(manifest.Version{}); !got.Equals(a) {
		t.Fatalf("merge with the zero version = %q, want %q", got, a)
	}
	if want := devC.String() + ":3"; !strings.Contains(merged.String(), want) {
		t.Fatalf("merge %q does not carry %q", merged, want)
	}
}

func TestVersionBump(t *testing.T) {
	a := version(t, manifest.Counter{ID: devA, N: 2})

	raised := a.Bump(devA)
	if raised.Counter(devA) != 3 {
		t.Fatalf("bump = %q, want the counter at 3", raised)
	}
	if a.Counter(devA) != 2 {
		t.Fatalf("bump changed the receiver: %q", a)
	}
	if raised.Compare(a) != manifest.After {
		t.Fatal("a bumped version must dominate the one it came from")
	}

	added := a.Bump(devB)
	if added.Counter(devB) != 1 || added.Counter(devA) != 2 {
		t.Fatalf("bump of a new device = %q", added)
	}
	if got := a.Bump(""); !got.Equals(a) {
		t.Fatalf("bump of the empty device = %q, want %q", got, a)
	}
	if got := (manifest.Version{}).Bump(devA); got.Counter(devA) != 1 {
		t.Fatalf("bump of the zero version = %q", got)
	}
}

func TestVersionStringRoundTrip(t *testing.T) {
	if got := (manifest.Version{}).String(); got != "" {
		t.Fatalf("zero version string = %q, want empty", got)
	}
	v, err := manifest.ParseVersion("")
	if err != nil || !v.IsZero() {
		t.Fatalf("ParseVersion(\"\") = %q, %v", v, err)
	}

	// The input names the devices out of order; the vector is stored and
	// written in device-id order.
	unordered := version(t, manifest.Counter{ID: devC, N: 3}, manifest.Counter{ID: devA, N: 1})
	want := devA.String() + ":1," + devC.String() + ":3"
	if got := unordered.String(); got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	back, err := manifest.ParseVersion(want)
	if err != nil {
		t.Fatalf("ParseVersion error: %v", err)
	}
	if !back.Equals(unordered) {
		t.Fatalf("round trip = %q, want %q", back, unordered)
	}
}

func TestVersionRefusesBadCounters(t *testing.T) {
	cases := map[string][]manifest.Counter{
		"invalid device id": {{ID: "nope", N: 1}},
		"zero counter":      {{ID: devA, N: 0}},
		"duplicate device":  {{ID: devA, N: 1}, {ID: devA, N: 2}},
	}
	for name, counters := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := manifest.NewVersion(counters); err == nil {
				t.Fatal("NewVersion accepted the counters")
			}
		})
	}
	for _, s := range []string{devA.String() + ":0", devA.String() + ":1," + devA.String() + ":2", devA.String(), devA.String() + ":x", ":1"} {
		if _, err := manifest.ParseVersion(s); err == nil {
			t.Fatalf("ParseVersion(%q) accepted it", s)
		}
	}
	// A stored vector carries whatever id the device that wrote it had, so
	// reading one back does not check that the id is a real device id.
	if _, err := manifest.ParseVersion("SHORTID:1"); err != nil {
		t.Fatalf("ParseVersion of a stored short id: %v", err)
	}
}

func TestVersionCountersDoNotAlias(t *testing.T) {
	v := version(t, manifest.Counter{ID: devA, N: 1})
	counters := v.Counters()
	counters[0].N = 9
	if v.Counter(devA) != 1 {
		t.Fatalf("Counters aliases the version: %q", v)
	}
}

func TestContentDiffers(t *testing.T) {
	base := manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Mode: 0o644, Hash: manifest.Hash{1}}

	cases := []struct {
		name string
		with func(e *manifest.Entry)
		want bool
	}{
		{"mode only", func(e *manifest.Entry) { e.Mode = 0o600 }, false},
		{"size only", func(e *manifest.Entry) { e.Size = 12 }, false},
		{"hash", func(e *manifest.Entry) { e.Hash = manifest.Hash{2} }, true},
		{"kind", func(e *manifest.Entry) { e.Kind = manifest.KindDir }, true},
		{"link target", func(e *manifest.Entry) { e.LinkTarget = "elsewhere" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.with(&other)
			if got := manifest.ContentDiffers(base, other); got != tc.want {
				t.Fatalf("ContentDiffers = %v, want %v", got, tc.want)
			}
		})
	}
	if manifest.ContentDiffers(base, base) {
		t.Fatal("an entry differs from itself")
	}
}

func TestVersion_Raise(t *testing.T) {
	base := version(t, manifest.Counter{ID: devA, N: 2}, manifest.Counter{ID: devB, N: 5})
	cases := []struct {
		name string
		in   manifest.Version
		id   device.ID
		to   uint64
		want string
	}{
		{name: "raises a counter that is behind", in: base, id: devA, to: 7, want: string(devA) + ":7," + string(devB) + ":5"},
		{name: "keeps a counter that is ahead", in: base, id: devB, to: 3, want: string(devA) + ":2," + string(devB) + ":5"},
		{name: "adds a device that is not in the vector", in: base, id: devC, to: 4, want: string(devA) + ":2," + string(devB) + ":5," + string(devC) + ":4"},
		{name: "a zero floor changes nothing", in: base, id: devA, to: 0, want: string(devA) + ":2," + string(devB) + ":5"},
		{name: "an empty id changes nothing", in: base, id: "", to: 9, want: string(devA) + ":2," + string(devB) + ":5"},
		{name: "raises the zero vector", in: manifest.Version{}, id: devA, to: 3, want: string(devA) + ":3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Raise(tc.id, tc.to).String(); got != tc.want {
				t.Fatalf("Raise(%q, %d) = %q, want %q", tc.id, tc.to, got, tc.want)
			}
		})
	}
}
