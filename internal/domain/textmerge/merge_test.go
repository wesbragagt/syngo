package textmerge_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/textmerge"
)

// TestMerge_DisjointRegionsTakeBothEdits is the case the feature exists for:
// two devices edited different parts of the same document and neither edit is
// lost.
func TestMerge_DisjointRegionsTakeBothEdits(t *testing.T) {
	base := "title\nalpha\nbeta\ngamma\ndelta\n"
	local := "title\nALPHA\nbeta\ngamma\ndelta\n"
	remote := "title\nalpha\nbeta\ngamma\nDELTA\n"

	merged, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "title\nALPHA\nbeta\ngamma\nDELTA\n"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_InsertionsFarApartAreBothKept covers added lines rather than
// changed ones, which are the hunks with an empty base range.
func TestMerge_InsertionsFarApartAreBothKept(t *testing.T) {
	base := "one\ntwo\nthree\nfour\nfive\nsix\n"
	local := "zero\none\ntwo\nthree\nfour\nfive\nsix\n"
	remote := "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"

	merged, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "zero\none\ntwo\nthree\nfour\nfive\nsix\nseven\n"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_DeletionOnOneSideAndEditOnTheOther mixes the two kinds of hunk.
func TestMerge_DeletionOnOneSideAndEditOnTheOther(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	local := "a\nc\nd\ne\n"     // b removed
	remote := "a\nb\nc\nd\nE\n" // e changed

	merged, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "a\nc\nd\nE\n"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_SameLineChangedBothWaysFallsBack is the case that must never be
// merged: there is no answer, so the caller keeps both versions.
func TestMerge_SameLineChangedBothWaysFallsBack(t *testing.T) {
	base := "one\ntwo\nthree\n"
	local := "one\nLOCAL\nthree\n"
	remote := "one\nREMOTE\nthree\n"

	if _, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote)); !errors.Is(err, textmerge.ErrOverlap) {
		t.Fatalf("merge error = %v, want %v", err, textmerge.ErrOverlap)
	}
}

// TestMerge_AdjacentEditsFallBack holds the rule that two changes need an
// untouched base line between them. Interleaving them would be a guess.
func TestMerge_AdjacentEditsFallBack(t *testing.T) {
	base := "one\ntwo\nthree\nfour\n"
	local := "one\nLOCAL\nthree\nfour\n"
	remote := "one\ntwo\nREMOTE\nfour\n"

	if _, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote)); !errors.Is(err, textmerge.ErrOverlap) {
		t.Fatalf("merge error = %v, want %v", err, textmerge.ErrOverlap)
	}
}

// TestMerge_TwoInsertionsAtOnePlaceFallBack: nothing says which of the two
// inserted lines comes first, so nothing is merged.
func TestMerge_TwoInsertionsAtOnePlaceFallBack(t *testing.T) {
	base := "one\ntwo\n"
	local := "one\nLOCAL\ntwo\n"
	remote := "one\nREMOTE\ntwo\n"

	if _, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote)); !errors.Is(err, textmerge.ErrOverlap) {
		t.Fatalf("merge error = %v, want %v", err, textmerge.ErrOverlap)
	}
}

// TestMerge_TheSameEditOnBothSidesIsNotAConflict: two devices that made one
// and the same change agree, and the result is that change applied once.
func TestMerge_TheSameEditOnBothSidesIsNotAConflict(t *testing.T) {
	base := "one\ntwo\nthree\nfour\n"
	local := "one\nTWO\nthree\nFOUR\n"
	remote := "one\nTWO\nthree\nfour\n"

	merged, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "one\nTWO\nthree\nFOUR\n"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_BinaryContentIsNeverMerged covers a NUL byte on each of the three
// sides, because a merge of binary content is the outcome this must never
// produce.
func TestMerge_BinaryContentIsNeverMerged(t *testing.T) {
	text := []byte("one\ntwo\nthree\n")
	binary := []byte("one\ntwo\x00\nthree\n")
	for _, c := range []struct {
		name                string
		base, local, remote []byte
	}{
		{"base", binary, text, []byte("one\ntwo\nTHREE\n")},
		{"local", text, binary, []byte("one\ntwo\nTHREE\n")},
		{"remote", text, []byte("ONE\ntwo\nthree\n"), binary},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := textmerge.Merge(c.base, c.local, c.remote); !errors.Is(err, textmerge.ErrNotText) {
				t.Fatalf("merge error = %v, want %v", err, textmerge.ErrNotText)
			}
		})
	}
}

// TestMerge_InvalidUTF8IsNotText: content that is not valid UTF-8 is not text
// this merges, even without a NUL byte in it.
func TestMerge_InvalidUTF8IsNotText(t *testing.T) {
	base := []byte("one\ntwo\n")
	local := []byte("one\n\xff\xfe\n")
	if textmerge.IsText(local) {
		t.Fatal("IsText said invalid UTF-8 is text")
	}
	if _, err := textmerge.Merge(base, local, []byte("ONE\ntwo\n")); !errors.Is(err, textmerge.ErrNotText) {
		t.Fatalf("merge error = %v, want %v", err, textmerge.ErrNotText)
	}
}

// TestMerge_AboveTheSizeLimitFallsBack: a file larger than MaxSize is not
// merged whatever it holds.
func TestMerge_AboveTheSizeLimitFallsBack(t *testing.T) {
	big := []byte(strings.Repeat("a line of text\n", textmerge.MaxSize/15+2))
	if len(big) <= textmerge.MaxSize {
		t.Fatalf("test input is %d bytes, not above the %d limit", len(big), textmerge.MaxSize)
	}
	local := append([]byte("first\n"), big...)
	remote := append(big, []byte("last\n")...)

	if _, err := textmerge.Merge(big, local, remote); !errors.Is(err, textmerge.ErrTooLarge) {
		t.Fatalf("merge error = %v, want %v", err, textmerge.ErrTooLarge)
	}
}

// TestMerge_ARewriteFallsBack: two sides further from the base than MaxEdits
// are not an edit any more, and the merge gives up rather than work on them.
func TestMerge_ARewriteFallsBack(t *testing.T) {
	var base, local, remote bytes.Buffer
	for i := range textmerge.MaxEdits + 10 {
		base.WriteString("base line\n")
		local.WriteString("local line ")
		local.WriteString(string(rune('a' + i%26)))
		local.WriteString("\n")
		remote.WriteString("base line\n")
	}
	remote.WriteString("one appended line\n")

	if _, err := textmerge.Merge(base.Bytes(), local.Bytes(), remote.Bytes()); !errors.Is(err, textmerge.ErrTooDifferent) {
		t.Fatalf("merge error = %v, want %v", err, textmerge.ErrTooDifferent)
	}
}

// TestMerge_NoFinalNewlineRoundTrips: the last line has no terminator and must
// not gain one.
func TestMerge_NoFinalNewlineRoundTrips(t *testing.T) {
	base := "a\nb\nc\nd"
	local := "A\nb\nc\nd"
	remote := "a\nb\nc\nD"

	merged, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "A\nb\nc\nD"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_CRLFLineEndingsAreKept: a terminator is part of its line, so
// nothing rewrites it.
func TestMerge_CRLFLineEndingsAreKept(t *testing.T) {
	base := "a\r\nb\r\nc\r\nd\r\n"
	local := "A\r\nb\r\nc\r\nd\r\n"
	remote := "a\r\nb\r\nc\r\nD\r\n"

	merged, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "A\r\nb\r\nc\r\nD\r\n"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_OneSideUnchanged takes the other side whole, which is the case a
// conflict never reaches but the algorithm still has to get right.
func TestMerge_OneSideUnchanged(t *testing.T) {
	base := "a\nb\n"
	merged, err := textmerge.Merge([]byte(base), []byte(base), []byte("a\nB\nc\n"))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "a\nB\nc\n"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_BlockEditsInDifferentPlaces covers hunks of several lines each.
func TestMerge_BlockEditsInDifferentPlaces(t *testing.T) {
	base := "1\n2\n3\n4\n5\n6\n7\n8\n9\n"
	local := "1\ntwo\nthree\n4\n5\n6\n7\n8\n9\n"
	remote := "1\n2\n3\n4\n5\n6\nseven\neight\nnine\n"

	merged, err := textmerge.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := "1\ntwo\nthree\n4\n5\n6\nseven\neight\nnine\n"; string(merged) != want {
		t.Fatalf("merged = %q, want %q", merged, want)
	}
}

// TestMerge_EmptyBase: both sides added content to a file that had none. They
// have no base line between them, so it is a conflict.
func TestMerge_EmptyBase(t *testing.T) {
	if _, err := textmerge.Merge(nil, []byte("local\n"), []byte("remote\n")); !errors.Is(err, textmerge.ErrOverlap) {
		t.Fatalf("merge error = %v, want %v", err, textmerge.ErrOverlap)
	}
}

// TestMerge_IdenticalSidesAreNotAConflict: the same content on both sides
// comes back unchanged whatever it holds, binary included, because there is
// nothing to decide.
func TestMerge_IdenticalSidesAreNotAConflict(t *testing.T) {
	both := []byte("a\nb\n")
	merged, err := textmerge.Merge([]byte("a\n"), both, both)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !bytes.Equal(merged, both) {
		t.Fatalf("merged = %q, want %q", merged, both)
	}
}
