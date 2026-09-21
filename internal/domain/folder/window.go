package folder

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// minutesPerDay is how many minutes a local day nominally has. A day that
// gains or loses an hour to daylight saving still has the same wall-clock
// minutes, which is what a window is written in.
const minutesPerDay = 24 * 60

// ErrInvalidWindow is returned when a sync window cannot be read or is empty.
var ErrInvalidWindow = errors.New("invalid sync window")

// Minute is a wall-clock minute of a local day, 0 to 1439.
type Minute int

// String renders the minute as HH:MM.
func (m Minute) String() string { return fmt.Sprintf("%02d:%02d", int(m)/60, int(m)%60) }

// Window is a span of local wall-clock time a folder may pull in. The end is
// exclusive, and a start after the end wraps past midnight, so 22:00-06:00 is
// the night.
type Window struct {
	Start, End Minute
}

// ParseWindow reads "HH:MM-HH:MM" in local wall-clock time.
func ParseWindow(s string) (Window, error) {
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("%w %q: want HH:MM-HH:MM", ErrInvalidWindow, s)
	}
	from, err := parseMinute(start)
	if err != nil {
		return Window{}, fmt.Errorf("%w %q: %v", ErrInvalidWindow, s, err)
	}
	to, err := parseMinute(end)
	if err != nil {
		return Window{}, fmt.Errorf("%w %q: %v", ErrInvalidWindow, s, err)
	}
	w := Window{Start: from, End: to}
	if err := w.Validate(); err != nil {
		return Window{}, fmt.Errorf("%w %q: %v", ErrInvalidWindow, s, err)
	}
	return w, nil
}

// parseMinute reads one HH:MM clock time.
func parseMinute(s string) (Minute, error) {
	hh, mm, ok := strings.Cut(s, ":")
	if !ok {
		return 0, errors.New("want HH:MM")
	}
	hours, err := strconv.Atoi(hh)
	if err != nil || hours < 0 || hours > 23 || len(hh) == 0 || len(hh) > 2 {
		return 0, fmt.Errorf("hour %q is not 00 to 23", hh)
	}
	minutes, err := strconv.Atoi(mm)
	if err != nil || minutes < 0 || minutes > 59 || len(mm) != 2 {
		return 0, fmt.Errorf("minute %q is not 00 to 59", mm)
	}
	return Minute(hours*60 + minutes), nil
}

// Validate reports whether the window spans any time at all.
func (w Window) Validate() error {
	if w.Start < 0 || w.Start >= minutesPerDay || w.End < 0 || w.End >= minutesPerDay {
		return errors.New("a window bound is outside 00:00 to 23:59")
	}
	if w.Start == w.End {
		return errors.New("a window that starts when it ends is never open")
	}
	return nil
}

// String renders the window in its canonical form, "22:00-06:00".
func (w Window) String() string { return w.Start.String() + "-" + w.End.String() }

// Contains reports whether t falls in the window. The wall-clock fields of t
// are read in t's own location, so the caller decides which zone the window is
// written in and the domain never touches time.Local.
func (w Window) Contains(t time.Time) bool {
	m := Minute(t.Hour()*60 + t.Minute())
	if w.Start < w.End {
		return m >= w.Start && m < w.End
	}
	return m >= w.Start || m < w.End
}

// NextStart returns the first opening strictly after t, in t's location. A
// start that falls in an hour daylight saving skips is normalized by
// time.Date, so the window opens when the wall clock reaches the normalized
// time.
func (w Window) NextStart(t time.Time) time.Time {
	year, month, day := t.Date()
	start := time.Date(year, month, day, int(w.Start)/60, int(w.Start)%60, 0, 0, t.Location())
	if !start.After(t) {
		start = time.Date(year, month, day+1, int(w.Start)/60, int(w.Start)%60, 0, 0, t.Location())
	}
	return start
}

// Windows is the set of spans a folder pulls in. An empty set means the folder
// is always open.
type Windows []Window

// ParseWindows reads a list of "HH:MM-HH:MM" spans, refusing duplicates once
// they are canonical.
func ParseWindows(ss []string) (Windows, error) {
	out := make(Windows, 0, len(ss))
	seen := make(map[Window]struct{}, len(ss))
	for _, s := range ss {
		w, err := ParseWindow(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		if _, ok := seen[w]; ok {
			return nil, fmt.Errorf("%w: %s is given twice", ErrInvalidWindow, w)
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Always reports whether the folder is open at every time.
func (ws Windows) Always() bool { return len(ws) == 0 }

// Contains reports whether t falls in any window.
func (ws Windows) Contains(t time.Time) bool {
	if ws.Always() {
		return true
	}
	for _, w := range ws {
		if w.Contains(t) {
			return true
		}
	}
	return false
}

// Next returns the first opening after t. It reports false when the folder is
// always open or is open at t already, because neither has a next opening.
func (ws Windows) Next(t time.Time) (time.Time, bool) {
	if ws.Contains(t) {
		return time.Time{}, false
	}
	var next time.Time
	for _, w := range ws {
		start := w.NextStart(t)
		if next.IsZero() || start.Before(next) {
			next = start
		}
	}
	return next, !next.IsZero()
}

// Strings renders every window in its canonical form.
func (ws Windows) Strings() []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.String())
	}
	return out
}

// Validate reports whether every window is usable and none is repeated.
func (ws Windows) Validate() error {
	seen := make(map[Window]struct{}, len(ws))
	for _, w := range ws {
		if err := w.Validate(); err != nil {
			return fmt.Errorf("%w %s: %v", ErrInvalidWindow, w, err)
		}
		if _, ok := seen[w]; ok {
			return fmt.Errorf("%w: %s is given twice", ErrInvalidWindow, w)
		}
		seen[w] = struct{}{}
	}
	return nil
}

// EncodeWindows renders the windows as the comma separated string the folder
// row holds. An always open folder encodes to the empty string.
func EncodeWindows(ws Windows) string { return strings.Join(ws.Strings(), ",") }

// DecodeWindows reads what EncodeWindows wrote.
func DecodeWindows(s string) (Windows, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	return ParseWindows(strings.Split(s, ","))
}
