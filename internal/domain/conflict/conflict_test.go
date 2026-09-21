package conflict_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/conflict"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// at is the detection time every name test uses.
var at = time.Date(2026, time.September, 21, 15, 30, 0, 0, time.UTC)

// peer is the device a conflict is detected with; only its first 7 characters
// reach the name.
const peer = device.ID("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRST")

func TestName(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{"keeps the extension", "report.md", "report.sync-conflict-20260921-153000-ABCDEFG.md"},
		{"keeps the directory", "notes/report.md", "notes/report.sync-conflict-20260921-153000-ABCDEFG.md"},
		{"without an extension", "README", "README.sync-conflict-20260921-153000-ABCDEFG"},
		{"only the last extension", "archive.tar.gz", "archive.tar.sync-conflict-20260921-153000-ABCDEFG.gz"},
		{"a dot file has no extension", ".bashrc", ".bashrc.sync-conflict-20260921-153000-ABCDEFG"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conflict.Name(tc.rel, at, peer); got != tc.want {
				t.Fatalf("Name(%q) = %q, want %q", tc.rel, got, tc.want)
			}
		})
	}
}

func TestName_UsesUTCWhateverTheZone(t *testing.T) {
	zone := time.FixedZone("UTC+5", 5*60*60)
	got := conflict.Name("a.txt", at.In(zone), peer)
	if want := conflict.Name("a.txt", at, peer); got != want {
		t.Fatalf("Name in %s = %q, want %q", zone, got, want)
	}
}

func TestWithCounter_GoesBeforeTheExtension(t *testing.T) {
	name := conflict.Name("report.md", at, peer)
	got := conflict.WithCounter(name, 2)
	want := "report.sync-conflict-20260921-153000-ABCDEFG-2.md"
	if got != want {
		t.Fatalf("WithCounter = %q, want %q", got, want)
	}
	if !conflict.IsCopy(got) {
		t.Fatalf("%q is not recognized as a conflict copy", got)
	}
}

func TestIsCopy(t *testing.T) {
	if conflict.IsCopy("report.md") {
		t.Fatal("an ordinary file is reported as a conflict copy")
	}
	if !conflict.IsCopy(conflict.Name("sub/report.md", at, peer)) {
		t.Fatal("a conflict copy is not recognized")
	}
}

func TestKeepsPath_IsTheSameAnswerOnBothSides(t *testing.T) {
	var low, high manifest.Hash
	low[0], high[0] = 1, 2

	if conflict.KeepsPath(low, high) {
		t.Fatal("the smaller version keeps the path")
	}
	if !conflict.KeepsPath(high, low) {
		t.Fatal("the greater version does not keep the path")
	}
	// The peer runs the same rule with the roles swapped; exactly one side
	// must move its version aside.
	if conflict.KeepsPath(low, high) == conflict.KeepsPath(high, low) {
		t.Fatal("both sides reach the same decision about their own version")
	}
	if !conflict.KeepsPath(high, high) {
		t.Fatal("identical content should keep the path")
	}
}

func TestRecordID_DependsOnWhatDivergedAndNotOnTime(t *testing.T) {
	var local, remote manifest.Hash
	local[0], remote[0] = 9, 8

	first := conflict.RecordID("docs", "a.txt", local, remote, peer)
	if first != conflict.RecordID("docs", "a.txt", local, remote, peer) {
		t.Fatal("the same divergence produced two ids")
	}
	if len(first) != 32 || strings.ContainsAny(first, "ghijklmnopqrstuvwxyz") {
		t.Fatalf("id = %q, want 32 hex characters", first)
	}
	if first == conflict.RecordID("docs", "b.txt", local, remote, peer) {
		t.Fatal("two paths share one id")
	}
	if first == conflict.RecordID("docs", "a.txt", remote, local, peer) {
		t.Fatal("the two versions are interchangeable in the id")
	}
}
