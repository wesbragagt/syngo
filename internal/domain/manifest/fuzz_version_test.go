package manifest_test

import (
	"strings"
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// FuzzParseVersion checks that a parsed version vector round-trips through
// String and that its counters are normalized: sorted by device id, none zero
// and none repeated.
func FuzzParseVersion(f *testing.F) {
	seeds := []string{
		"",
		"  ",
		string(devA) + ":1",
		string(devA) + ":1," + string(devB) + ":2",
		string(devB) + ":2," + string(devA) + ":1",
		string(devA) + ":18446744073709551615",
		// Known-bad shapes.
		string(devA),
		":1",
		string(devA) + ":",
		string(devA) + ":0",
		string(devA) + ":1," + string(devA) + ":2",
		string(devA) + ":-1",
		string(devA) + ":18446744073709551616",
		string(devA) + ":1,",
		",",
		string(devA) + ":1:2",
		"\x00:1",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		v, err := manifest.ParseVersion(s)
		if err != nil {
			return
		}

		// Round trip: the form String writes parses back to the same
		// vector, which is what the store relies on.
		text := v.String()
		again, err := manifest.ParseVersion(text)
		if err != nil {
			t.Fatalf("ParseVersion(%q) produced %q, which does not parse: %v", s, text, err)
		}
		if again.String() != text {
			t.Fatalf("ParseVersion(%q): %q round-trips to %q", s, text, again.String())
		}
		if v.Compare(again) != manifest.Equal {
			t.Fatalf("ParseVersion(%q): %q does not compare equal to itself", s, text)
		}

		// Normalization: sorted, no zero counter, no repeated device.
		counters := v.Counters()
		seen := make(map[string]bool, len(counters))
		for i, c := range counters {
			if c.N == 0 {
				t.Fatalf("ParseVersion(%q) kept a zero counter for %q", s, c.ID)
			}
			if c.ID == "" {
				t.Fatalf("ParseVersion(%q) kept a counter without a device id", s)
			}
			if strings.ContainsAny(string(c.ID), ":,") {
				t.Fatalf("ParseVersion(%q) kept the separator-bearing id %q", s, c.ID)
			}
			if seen[string(c.ID)] {
				t.Fatalf("ParseVersion(%q) kept %q twice", s, c.ID)
			}
			seen[string(c.ID)] = true
			if i > 0 && counters[i-1].ID >= c.ID {
				t.Fatalf("ParseVersion(%q) counters are not sorted: %q then %q", s, counters[i-1].ID, c.ID)
			}
		}
	})
}

// TestParseVersionLeadingSpaceDeviceIDDefect confirms the defect has been
// fixed: device ids with leading or trailing whitespace are now rejected.
// The defect was that ParseVersion used to accept them, String would output
// them as-is, but parsing the output again would fail because TrimSpace on
// the whole string would consume the whitespace, leaving an empty device id.
// This test confirms the fuzz seed "0:1, :1" is now rejected at parse time.
func TestParseVersionLeadingSpaceDeviceIDDefect(t *testing.T) {
	const input = "0:1, :1"

	_, err := manifest.ParseVersion(input)
	if err == nil {
		t.Fatalf("ParseVersion(%q) accepted a device id with leading whitespace; defect persists", input)
	}
	if !strings.Contains(err.Error(), "leading or trailing whitespace") {
		t.Fatalf("ParseVersion(%q) error %q does not mention whitespace", input, err)
	}
}
