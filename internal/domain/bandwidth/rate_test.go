package bandwidth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/bandwidth"
)

func TestParseRate_Table(t *testing.T) {
	cases := []struct {
		in      string
		want    bandwidth.Rate
		wantErr bool
	}{
		{in: "0", want: bandwidth.Unlimited},
		{in: "unlimited", want: bandwidth.Unlimited},
		{in: "UNLIMITED", want: bandwidth.Unlimited},
		{in: "4194304", want: 4 << 20},
		{in: "4MiB", want: 4 << 20},
		{in: "4mib", want: 4 << 20},
		{in: "4MiB/s", want: 4 << 20},
		{in: "500KB", want: 500_000},
		{in: "1.5GiB", want: 1610612736},
		{in: "1MB", want: 1_000_000},
		{in: "512B", want: 512},
		{in: " 4MiB ", want: 4 << 20},
		{in: "-1", wantErr: true},
		{in: "4 MiB", wantErr: true},
		{in: "MiB", wantErr: true},
		{in: "1e3", wantErr: true},
		{in: "", wantErr: true},
		{in: "4TiB", wantErr: true},
		{in: "1.2.3", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := bandwidth.ParseRate(tc.in)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("ParseRate(%q) = %d, want an error", tc.in, got)
			case tc.wantErr:
				if !errors.Is(err, bandwidth.ErrInvalidRate) {
					t.Fatalf("ParseRate(%q) error = %v, want ErrInvalidRate", tc.in, err)
				}
			case err != nil:
				t.Fatalf("ParseRate(%q): %v", tc.in, err)
			case got != tc.want:
				t.Fatalf("ParseRate(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestRate_String(t *testing.T) {
	cases := []struct {
		rate bandwidth.Rate
		want string
	}{
		{bandwidth.Unlimited, "unlimited"},
		{4 << 20, "4 MiB/s"},
		{500_000, "500 kB/s"},
		{1 << 30, "1 GiB/s"},
		{1_000_000, "1 MB/s"},
		{999, "999 B/s"},
		{1 << 10, "1 KiB/s"},
	}
	for _, tc := range cases {
		if got := tc.rate.String(); got != tc.want {
			t.Errorf("Rate(%d).String() = %q, want %q", tc.rate, got, tc.want)
		}
	}
}

func TestRate_TimeFor(t *testing.T) {
	rate := bandwidth.Rate(4 << 20)
	if got := rate.TimeFor(4 << 20); got != time.Second {
		t.Errorf("TimeFor(4 MiB) = %v, want 1s", got)
	}
	if got := rate.TimeFor(20 << 20); got != 5*time.Second {
		t.Errorf("TimeFor(20 MiB) = %v, want 5s", got)
	}
	if got := bandwidth.Unlimited.TimeFor(20 << 20); got != 0 {
		t.Errorf("unlimited TimeFor = %v, want 0", got)
	}
	if got := rate.TimeFor(0); got != 0 {
		t.Errorf("TimeFor(0) = %v, want 0", got)
	}
}
