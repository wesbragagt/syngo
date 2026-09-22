package manifest

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// ErrInvalidVersion reports counters that cannot form a version vector: an
// invalid device id, a zero counter or a device named twice.
var ErrInvalidVersion = errors.New("invalid version vector")

// Counter is one component of a Version: how many content changes a device has
// made to a path.
type Counter struct {
	ID device.ID
	N  uint64
}

// Version is a version vector: for each device, how many content changes it
// has made to a path. Two versions are ordered by dominance; two that neither
// dominate were made concurrently. The zero Version has no counters and is
// dominated by every non-empty one.
//
// A Version is a value: every method returns a new vector and none of them
// writes through the receiver, so an Entry copied by value never shares its
// counters with the copy.
type Version struct {
	counters []Counter // sorted by ID, no zero counters
}

// Ordering is the result of comparing two versions.
type Ordering uint8

// Orderings. Before and After are mirror images: Compare(a, b) == Before holds
// exactly when Compare(b, a) == After.
const (
	Equal      Ordering = iota // the same counters
	Before                     // the receiver is dominated: the argument is newer
	After                      // the receiver dominates: the receiver is newer
	Concurrent                 // neither dominates the other
)

// String returns the lowercase name of the ordering.
func (o Ordering) String() string {
	switch o {
	case Equal:
		return "equal"
	case Before:
		return "before"
	case After:
		return "after"
	case Concurrent:
		return "concurrent"
	default:
		return "unknown"
	}
}

// NewVersion builds a normalized version from counters. It fails on an invalid
// device id, a zero counter or a repeated id.
func NewVersion(counters []Counter) (Version, error) {
	if len(counters) == 0 {
		return Version{}, nil
	}
	out := make([]Counter, 0, len(counters))
	seen := make(map[device.ID]bool, len(counters))
	for _, c := range counters {
		id, err := device.ParseID(string(c.ID))
		if err != nil {
			return Version{}, fmt.Errorf("%w: %w", ErrInvalidVersion, err)
		}
		if c.N == 0 {
			return Version{}, fmt.Errorf("%w: device %s has a zero counter", ErrInvalidVersion, id.Short())
		}
		if seen[id] {
			return Version{}, fmt.Errorf("%w: device %s appears twice", ErrInvalidVersion, id.Short())
		}
		seen[id] = true
		out = append(out, Counter{ID: id, N: c.N})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return Version{counters: out}, nil
}

// Counters returns the components in device-id order. The result is a copy:
// writing to it does not change the version.
func (v Version) Counters() []Counter {
	if len(v.counters) == 0 {
		return nil
	}
	out := make([]Counter, len(v.counters))
	copy(out, v.counters)
	return out
}

// IsZero reports whether the version has no counters, which is what a path
// that predates version vectors carries.
func (v Version) IsZero() bool { return len(v.counters) == 0 }

// Counter returns the counter of id, zero when the device never changed the
// path.
func (v Version) Counter(id device.ID) uint64 {
	for _, c := range v.counters {
		if c.ID == id {
			return c.N
		}
	}
	return 0
}

// Bump returns v with the counter of id raised by one. An empty id returns v
// unchanged, so a device without an identity records no version.
func (v Version) Bump(id device.ID) Version {
	if id == "" {
		return v
	}
	out := make([]Counter, 0, len(v.counters)+1)
	raised := false
	for _, c := range v.counters {
		if c.ID == id {
			out = append(out, Counter{ID: id, N: c.N + 1})
			raised = true
			continue
		}
		out = append(out, c)
	}
	if !raised {
		out = append(out, Counter{ID: id, N: 1})
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	return Version{counters: out}
}

// Merge returns the component-wise maximum of v and o, which dominates both.
func (v Version) Merge(o Version) Version {
	out := make([]Counter, 0, len(v.counters)+len(o.counters))
	i, j := 0, 0
	for i < len(v.counters) && j < len(o.counters) {
		a, b := v.counters[i], o.counters[j]
		switch {
		case a.ID < b.ID:
			out = append(out, a)
			i++
		case a.ID > b.ID:
			out = append(out, b)
			j++
		default:
			out = append(out, Counter{ID: a.ID, N: max(a.N, b.N)})
			i++
			j++
		}
	}
	out = append(out, v.counters[i:]...)
	out = append(out, o.counters[j:]...)
	if len(out) == 0 {
		return Version{}
	}
	return Version{counters: out}
}

// Compare orders v against o by dominance.
func (v Version) Compare(o Version) Ordering {
	var less, greater bool
	i, j := 0, 0
	for i < len(v.counters) && j < len(o.counters) {
		a, b := v.counters[i], o.counters[j]
		switch {
		case a.ID < b.ID:
			greater = true // o has no counter for a.ID, which reads as 0
			i++
		case a.ID > b.ID:
			less = true
			j++
		default:
			if a.N < b.N {
				less = true
			} else if a.N > b.N {
				greater = true
			}
			i++
			j++
		}
	}
	if i < len(v.counters) {
		greater = true
	}
	if j < len(o.counters) {
		less = true
	}
	switch {
	case less && greater:
		return Concurrent
	case less:
		return Before
	case greater:
		return After
	default:
		return Equal
	}
}

// Equals reports whether v and o hold the same counters.
func (v Version) Equals(o Version) bool { return v.Compare(o) == Equal }

// String writes the version as "ID:N,ID:N" in device-id order. The zero
// version is the empty string. ParseVersion is its inverse.
func (v Version) String() string {
	var b strings.Builder
	for i, c := range v.counters {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(string(c.ID))
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(c.N, 10))
	}
	return b.String()
}

// ParseVersion reads the form String writes. An empty string is the zero
// version.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, nil
	}
	parts := strings.Split(s, ",")
	counters := make([]Counter, 0, len(parts))
	for _, part := range parts {
		id, digits, ok := strings.Cut(part, ":")
		if !ok {
			return Version{}, fmt.Errorf("%w: %q is not ID:N", ErrInvalidVersion, part)
		}
		n, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("%w: counter of %q: %w", ErrInvalidVersion, part, err)
		}
		counters = append(counters, Counter{ID: device.ID(id), N: n})
	}
	return NewVersion(counters)
}
