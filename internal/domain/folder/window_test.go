package folder_test

import (
	"errors"
	"testing"
	"time"
	// The DST cases below load Europe/Berlin. A bare CI image has no
	// zoneinfo, and LoadLocation would then fail rather than skip, so the
	// database is compiled in.
	_ "time/tzdata"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// berlin is the zone the daylight saving cases are pinned against.
func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load Europe/Berlin: %v", err)
	}
	return loc
}

func TestParseWindow_Canonical(t *testing.T) {
	w, err := folder.ParseWindow("9:05-17:30")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := w.String(); got != "09:05-17:30" {
		t.Fatalf("String() = %q, want %q", got, "09:05-17:30")
	}
}

func TestParseWindow_Rejects(t *testing.T) {
	for _, s := range []string{"25:00-01:00", "22:00", "22:00-22:00", "2200-0600", "22:60-01:00", "", "-", "22:0-01:00"} {
		if w, err := folder.ParseWindow(s); err == nil {
			t.Errorf("ParseWindow(%q) = %s, want an error", s, w)
		} else if !errors.Is(err, folder.ErrInvalidWindow) {
			t.Errorf("ParseWindow(%q) error = %v, want ErrInvalidWindow", s, err)
		}
	}
}

func TestWindow_ContainsOvernight(t *testing.T) {
	w, err := folder.ParseWindow("22:00-06:00")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		hour, minute int
		want         bool
	}{
		{22, 0, true}, {23, 30, true}, {3, 0, true}, {5, 59, true},
		{6, 0, false}, {12, 0, false},
	}
	for _, tc := range cases {
		at := time.Date(2026, time.June, 1, tc.hour, tc.minute, 0, 0, time.UTC)
		if got := w.Contains(at); got != tc.want {
			t.Errorf("Contains(%02d:%02d) = %v, want %v", tc.hour, tc.minute, got, tc.want)
		}
	}
}

func TestWindow_ContainsUsesTheTimesLocation(t *testing.T) {
	w, err := folder.ParseWindow("22:00-06:00")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The same instant: 20:30 UTC, which is 22:30 two hours east.
	instant := time.Date(2026, time.June, 1, 20, 30, 0, 0, time.UTC)
	if w.Contains(instant) {
		t.Error("the window contains 20:30 UTC, want it outside")
	}
	if !w.Contains(instant.In(time.FixedZone("+02", 2*60*60))) {
		t.Error("the window does not contain 22:30 at +02:00, want it inside")
	}
}

func TestWindows_NextCrossesMidnight(t *testing.T) {
	ws, err := folder.ParseWindows([]string{"22:00-06:00"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	at := time.Date(2026, time.June, 1, 23, 0, 0, 0, time.UTC)
	// 23:00 is inside the window, so there is no next opening.
	if next, ok := ws.Next(at); ok {
		t.Fatalf("Next inside the window = %s, true; want false", next)
	}
	// 07:00 is after it closed, so the next opening is tonight.
	next, ok := ws.Next(time.Date(2026, time.June, 1, 7, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("Next outside the window reported no opening")
	}
	if want := time.Date(2026, time.June, 1, 22, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("Next = %s, want %s", next, want)
	}
}

func TestWindows_NextInsideIsFalse(t *testing.T) {
	ws, err := folder.ParseWindows([]string{"09:00-17:00"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := ws.Next(time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)); ok {
		t.Fatal("a folder inside its window reports a next opening")
	}
	var always folder.Windows
	if _, ok := always.Next(time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)); ok {
		t.Fatal("an always open folder reports a next opening")
	}
}

func TestWindows_NextAcrossSpringForward(t *testing.T) {
	loc := berlin(t)
	ws, err := folder.ParseWindows([]string{"02:30-04:00"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 2026-03-29 02:00 to 03:00 does not exist in Berlin. A window that
	// starts at 02:30 opens when the wall clock reaches the normalized
	// time, 03:30 CEST, which is 01:30 UTC.
	next, ok := ws.Next(time.Date(2026, time.March, 29, 1, 0, 0, 0, loc))
	if !ok {
		t.Fatal("Next reported no opening on the spring forward night")
	}
	if want := time.Date(2026, time.March, 29, 1, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("Next = %s (%s UTC), want %s", next, next.UTC(), want)
	}
}

func TestWindows_ContainsAcrossFallBack(t *testing.T) {
	loc := berlin(t)
	ws, err := folder.ParseWindows([]string{"02:00-03:00"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 2026-10-25 02:00 to 03:00 happens twice in Berlin, once at +02:00
	// and once at +01:00. The window compares wall-clock minutes, so it
	// contains both and is an hour longer that night.
	first := time.Date(2026, time.October, 25, 0, 30, 0, 0, time.UTC).In(loc)
	second := time.Date(2026, time.October, 25, 1, 30, 0, 0, time.UTC).In(loc)
	if first.Hour() != 2 || second.Hour() != 2 {
		t.Fatalf("the repeated hour is %s and %s, want both at 02:30 local", first, second)
	}
	if !ws.Contains(first) || !ws.Contains(second) {
		t.Fatalf("Contains(%s) = %v, Contains(%s) = %v, want both true",
			first, ws.Contains(first), second, ws.Contains(second))
	}
}

func TestEncodeDecodeWindows_RoundTrip(t *testing.T) {
	ws, err := folder.ParseWindows([]string{"22:00-06:00", "12:00-13:00"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	encoded := folder.EncodeWindows(ws)
	if encoded != "22:00-06:00,12:00-13:00" {
		t.Fatalf("EncodeWindows = %q", encoded)
	}
	back, err := folder.DecodeWindows(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if folder.EncodeWindows(back) != encoded {
		t.Fatalf("round trip = %q, want %q", folder.EncodeWindows(back), encoded)
	}

	empty, err := folder.DecodeWindows(folder.EncodeWindows(nil))
	if err != nil {
		t.Fatalf("decode the empty column: %v", err)
	}
	if !empty.Always() {
		t.Fatalf("the empty column decodes to %v, want an always open folder", empty)
	}
}

func TestParseWindows_RejectsDuplicates(t *testing.T) {
	if _, err := folder.ParseWindows([]string{"22:00-06:00", "22:00-06:00"}); !errors.Is(err, folder.ErrInvalidWindow) {
		t.Fatalf("error = %v, want ErrInvalidWindow", err)
	}
}

func TestFolder_ValidateRejectsDuplicateWindows(t *testing.T) {
	f := folder.Folder{
		ID: "docs", Path: "/srv/docs", Mode: folder.ModeSendReceive,
		DeletePolicy: folder.DeletePropagate, Symlinks: folder.SymlinkSkip,
		SyncWindows: folder.Windows{{Start: 0, End: 60}, {Start: 0, End: 60}},
	}
	if err := f.Validate(); !errors.Is(err, folder.ErrInvalidWindow) {
		t.Fatalf("Validate = %v, want ErrInvalidWindow", err)
	}
	f.SyncWindows = folder.Windows{{Start: 60, End: 60}}
	if err := f.Validate(); !errors.Is(err, folder.ErrInvalidWindow) {
		t.Fatalf("Validate of an empty window = %v, want ErrInvalidWindow", err)
	}
}

func TestFolder_InWindow(t *testing.T) {
	f := folder.Folder{SyncWindows: folder.Windows{{Start: 22 * 60, End: 6 * 60}}}
	if f.InWindow(time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)) {
		t.Error("noon is inside a 22:00-06:00 window")
	}
	if !f.InWindow(time.Date(2026, time.June, 1, 23, 0, 0, 0, time.UTC)) {
		t.Error("23:00 is outside a 22:00-06:00 window")
	}
	var always folder.Folder
	if !always.InWindow(time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)) {
		t.Error("a folder without windows is not always open")
	}
}
