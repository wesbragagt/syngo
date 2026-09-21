// Package bandwidth defines the transfer rates a device limits itself to and
// how they are written down.
package bandwidth

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rate is a transfer rate in bytes per second. Zero means unlimited.
type Rate int64

// Unlimited is the rate that does not limit anything.
const Unlimited Rate = 0

// ErrInvalidRate is returned by ParseRate when the input is not a rate.
var ErrInvalidRate = errors.New("invalid rate")

// unit is one accepted suffix and how many bytes it stands for.
type unit struct {
	suffix string
	size   int64
}

// units are the suffixes ParseRate accepts, longest first so that "KiB" is
// matched before "K" would be. The decimal ones are powers of 1000, the binary
// ones powers of 1024, as in the rest of the industry.
var units = []unit{
	{"gib", 1 << 30},
	{"mib", 1 << 20},
	{"kib", 1 << 10},
	{"gb", 1000 * 1000 * 1000},
	{"mb", 1000 * 1000},
	{"kb", 1000},
	{"b", 1},
	{"", 1},
}

// ParseRate reads a rate: a whole number of bytes per second, or a number with
// one of the suffixes KB, MB, GB (decimal) or KiB, MiB, GiB (binary), case
// insensitive. A trailing "/s" is accepted and ignored, the number may have a
// decimal fraction, and "0" and "unlimited" both mean unlimited. Anything else,
// a negative number included, fails with ErrInvalidRate.
func ParseRate(s string) (Rate, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return 0, fmt.Errorf("%w: %q is empty", ErrInvalidRate, s)
	}
	if strings.EqualFold(text, "unlimited") {
		return Unlimited, nil
	}
	text = strings.TrimSuffix(strings.TrimSuffix(text, "/s"), "/S")

	number, suffix := splitNumber(text)
	if number == "" {
		return 0, fmt.Errorf("%w: %q has no number", ErrInvalidRate, s)
	}
	size, ok := unitSize(suffix)
	if !ok {
		return 0, fmt.Errorf("%w: %q is not a unit, want B, KB, MB, GB, KiB, MiB or GiB", ErrInvalidRate, suffix)
	}
	// ParseFloat would also accept "1e3" and "Inf"; splitNumber has already
	// refused everything but digits and one dot, so only the value is left
	// to check here.
	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %v", ErrInvalidRate, s, err)
	}
	bytes := value * float64(size)
	if bytes > float64(int64(1)<<62) {
		return 0, fmt.Errorf("%w: %q is too large", ErrInvalidRate, s)
	}
	return Rate(bytes), nil
}

// splitNumber splits text into its leading number and the rest. The number is
// digits with at most one dot; anything else leaves it empty, which is what
// makes "-1", "1e3" and "4 MiB" invalid.
func splitNumber(text string) (number, suffix string) {
	dots := 0
	end := 0
	for i, r := range text {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && dots == 0:
			dots++
		default:
			return text[:i], text[i:]
		}
		end = i + len(string(r))
	}
	return text[:end], text[end:]
}

// unitSize returns how many bytes a suffix stands for.
func unitSize(suffix string) (int64, bool) {
	lower := strings.ToLower(suffix)
	for _, u := range units {
		if lower == u.suffix {
			return u.size, true
		}
	}
	return 0, false
}

// Unlimited reports whether the rate does not limit anything.
func (r Rate) Unlimited() bool { return r <= 0 }

// String writes the rate the way a person reads it: binary units when the rate
// is a multiple of one, decimal units when it is a multiple of one of those,
// and plain bytes otherwise.
func (r Rate) String() string {
	if r.Unlimited() {
		return "unlimited"
	}
	for _, u := range units {
		if u.size > 1 && int64(r)%u.size == 0 {
			return fmt.Sprintf("%d %s/s", int64(r)/u.size, display(u.suffix))
		}
	}
	return fmt.Sprintf("%d B/s", int64(r))
}

// display returns the canonical spelling of a unit suffix.
func display(suffix string) string {
	switch suffix {
	case "kb":
		return "kB"
	case "mb":
		return "MB"
	case "gb":
		return "GB"
	case "kib":
		return "KiB"
	case "mib":
		return "MiB"
	case "gib":
		return "GiB"
	default:
		return "B"
	}
}

// TimeFor returns how long n bytes take at this rate, or zero when the rate is
// unlimited or n is not positive.
func (r Rate) TimeFor(n int64) time.Duration {
	if r.Unlimited() || n <= 0 {
		return 0
	}
	return time.Duration(float64(n) / float64(r) * float64(time.Second))
}
