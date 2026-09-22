package chunker_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/chunker"
)

// testCfg is the configuration the tests share: small enough that a test file
// is a handful of chunks, shaped like the production one.
var testCfg = chunker.ConfigFor(8 << 10)

// corpus returns deterministic pseudo-random bytes. A real file compresses and
// repeats; what matters here is that both sides see the same bytes.
func corpus(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 0x5eed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// split returns the spans of b, and fails the test if anything goes wrong.
func split(t *testing.T, b []byte, cfg chunker.Config) []chunker.Span {
	t.Helper()
	return splitReader(t, bytes.NewReader(b), cfg)
}

// splitReader returns the spans r is cut into.
func splitReader(t *testing.T, r io.Reader, cfg chunker.Config) []chunker.Span {
	t.Helper()
	var spans []chunker.Span
	err := chunker.Split(r, cfg, func(s chunker.Span, data []byte) error {
		if len(data) != s.Length {
			t.Fatalf("span %d says %d bytes, emit got %d", s.Index, s.Length, len(data))
		}
		spans = append(spans, s)
		return nil
	})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	return spans
}

// dribble is a reader that returns at most n bytes per Read, so a test can
// prove the cut points do not depend on how the bytes arrive.
type dribble struct {
	r io.Reader
	n int
}

func (d dribble) Read(p []byte) (int, error) {
	if len(p) > d.n {
		p = p[:d.n]
	}
	return d.r.Read(p)
}

// TestSplit_GoldenCutPoints pins the boundaries of a known input. Two devices
// only deduplicate while they cut identically, so the algorithm, the gear
// table, the masks and the normalization are frozen here: a change that moves
// a single boundary fails this test rather than quietly halving the savings of
// every mesh in the field.
func TestSplit_GoldenCutPoints(t *testing.T) {
	spans := split(t, corpus(200<<10, 1), testCfg)

	var got []int
	for _, s := range spans {
		got = append(got, s.Length)
	}
	want := []int{10036, 9143, 11116, 3176, 6377, 10262, 10823, 10274, 13549,
		10105, 11207, 17269, 9265, 12700, 10109, 10962, 9620, 5571,
		10827, 8227, 4182}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("cut points moved:\n got %v\nwant %v", got, want)
	}
}

// TestSplit_IsDeterministic is the property the golden test pins one case of:
// the same bytes cut the same way, whatever sizes the reader hands out. The
// receiving device reads from a socket and the sending one from a disk, so
// this is the difference between the two.
func TestSplit_IsDeterministic(t *testing.T) {
	data := corpus(300<<10, 2)
	want := split(t, data, testCfg)

	for _, n := range []int{1, 7, 512, 4093, 64 << 10, 1 << 20} {
		got := splitReader(t, dribble{r: bytes.NewReader(data), n: n}, testCfg)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("reads of %d bytes cut differently:\n got %v\nwant %v", n, got, want)
		}
	}
}

// TestSplit_CoversTheStreamExactly checks the spans are a partition: they
// start at zero, touch, and add up to the input.
func TestSplit_CoversTheStreamExactly(t *testing.T) {
	data := corpus(300<<10, 3)
	spans := split(t, data, testCfg)

	var offset int64
	for i, s := range spans {
		if s.Index != i {
			t.Fatalf("span %d carries index %d", i, s.Index)
		}
		if s.Offset != offset {
			t.Fatalf("span %d starts at %d, want %d", i, s.Offset, offset)
		}
		offset += int64(s.Length)
	}
	if offset != int64(len(data)) {
		t.Fatalf("spans cover %d bytes, the input has %d", offset, len(data))
	}
}

// TestSplit_SizeBounds is the promise the configuration makes: no chunk is
// longer than the maximum, and none is shorter than the minimum except the
// last, which is whatever the file has left.
func TestSplit_SizeBounds(t *testing.T) {
	for _, size := range []int{0, 1, 100, testCfg.Min - 1, testCfg.Min, testCfg.Max + 1, 1 << 20} {
		data := corpus(size, uint64(size)+7)
		spans := split(t, data, testCfg)
		if size == 0 && len(spans) != 0 {
			t.Fatalf("an empty stream produced %d chunks", len(spans))
		}
		for i, s := range spans {
			if s.Length > testCfg.Max {
				t.Fatalf("size %d: chunk %d is %d bytes, the maximum is %d",
					size, i, s.Length, testCfg.Max)
			}
			if s.Length < testCfg.Min && i != len(spans)-1 {
				t.Fatalf("size %d: chunk %d is %d bytes, the minimum is %d",
					size, i, s.Length, testCfg.Min)
			}
			if s.Length <= 0 {
				t.Fatalf("size %d: chunk %d is empty", size, i)
			}
		}
	}
}

// TestSplit_AverageIsNearTheConfiguredOne checks normalized chunking does what
// it is here for. A chunker that cut at the minimum or the maximum every time
// would satisfy the bounds test and deduplicate nothing.
func TestSplit_AverageIsNearTheConfiguredOne(t *testing.T) {
	spans := split(t, corpus(8<<20, 4), testCfg)
	if len(spans) == 0 {
		t.Fatal("no chunks")
	}
	avg := (8 << 20) / len(spans)
	if avg < testCfg.Avg/2 || avg > testCfg.Avg*2 {
		t.Fatalf("average chunk is %d bytes, want within a factor of two of %d", avg, testCfg.Avg)
	}
	atMax := 0
	for _, s := range spans {
		if s.Length == testCfg.Max {
			atMax++
		}
	}
	if atMax*10 > len(spans) {
		t.Fatalf("%d of %d chunks were truncated at the maximum; those do not deduplicate",
			atMax, len(spans))
	}
}

// TestSplit_BoundariesSurviveAnInsertion is the reason the package exists. One
// mebibyte is inserted near the front of a file and almost every chunk after
// the insertion is still cut where it was, so a receiver already holding the
// old file has to fetch only what changed.
func TestSplit_BoundariesSurviveAnInsertion(t *testing.T) {
	cfg := chunker.ConfigFor(64 << 10)
	original := corpus(16<<20, 5)
	inserted := corpus(1<<20, 6)

	at := 1 << 20
	edited := make([]byte, 0, len(original)+len(inserted))
	edited = append(edited, original[:at]...)
	edited = append(edited, inserted...)
	edited = append(edited, original[at:]...)

	before := hashes(split(t, original, cfg), original)
	after := hashes(split(t, edited, cfg), edited)

	shared := 0
	held := map[string]int{}
	for _, h := range before {
		held[h]++
	}
	for _, h := range after {
		if held[h] > 0 {
			held[h]--
			shared++
		}
	}

	// Everything but the inserted megabyte and the two chunks it lands
	// between should be recognized again.
	if shared < len(before)-4 {
		t.Fatalf("only %d of %d chunks survived the insertion", shared, len(before))
	}
	t.Logf("insertion of 1 MiB into 16 MiB: %d of %d chunks unchanged, %d new chunks",
		shared, len(before), len(after)-shared)
}

// hashes returns a comparable key per chunk: its bytes.
func hashes(spans []chunker.Span, data []byte) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, string(data[s.Offset:s.Offset+int64(s.Length)]))
	}
	return out
}

// TestSplit_FixedSizeWouldHaveMovedEverything is the comparison that makes the
// previous test mean something: the same insertion under fixed-size chunking
// invalidates every chunk after it.
func TestSplit_FixedSizeWouldHaveMovedEverything(t *testing.T) {
	const size = 1 << 20
	original := corpus(4<<20, 7)
	edited := append(append([]byte{}, original[:1000]...), original[1000-1:]...)

	same := 0
	for off := 0; off+size <= len(original) && off+size <= len(edited); off += size {
		if bytes.Equal(original[off:off+size], edited[off:off+size]) {
			same++
		}
	}
	if same > 1 {
		t.Fatalf("%d fixed-size chunks survived a one-byte insertion, expected at most the first", same)
	}
}

// TestConfigFor_DerivesUsableBounds checks the constructor never returns a
// configuration Validate refuses, whatever it is asked for.
func TestConfigFor_DerivesUsableBounds(t *testing.T) {
	for _, avg := range []int{-1, 0, 1, 512, 4 << 10, 1 << 20, 1 << 30} {
		cfg := chunker.ConfigFor(avg)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("ConfigFor(%d) = %+v: %v", avg, cfg, err)
		}
	}
}

// TestSplit_RefusesAnUnusableConfig checks a hand-built configuration is
// validated rather than producing chunks nothing can carry.
func TestSplit_RefusesAnUnusableConfig(t *testing.T) {
	bad := []chunker.Config{
		{Min: 0, Avg: 1024, Max: 4096},
		{Min: 1024, Avg: 512, Max: 4096},
		{Min: 1024, Avg: 4096, Max: 2048},
		{Min: 1024, Avg: 4096, Max: chunker.MaxSize + 1},
	}
	for _, cfg := range bad {
		err := chunker.Split(bytes.NewReader([]byte("x")), cfg, func(chunker.Span, []byte) error { return nil })
		if err == nil {
			t.Fatalf("Split accepted %+v", cfg)
		}
	}
}

// TestSplit_StopsAtTheFirstEmitFailure checks a caller that cannot store a
// chunk is not asked for the next one.
func TestSplit_StopsAtTheFirstEmitFailure(t *testing.T) {
	want := io.ErrClosedPipe
	seen := 0
	err := chunker.Split(bytes.NewReader(corpus(1<<20, 8)), testCfg, func(chunker.Span, []byte) error {
		seen++
		return want
	})
	if err != want {
		t.Fatalf("Split returned %v, want %v", err, want)
	}
	if seen != 1 {
		t.Fatalf("emit was called %d times after failing", seen)
	}
}
