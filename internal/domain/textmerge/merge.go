// Package textmerge merges two divergent versions of a text file against the
// version they both came from. It is line-based, it is pure, and it answers
// with a merged file only when the answer cannot be wrong.
//
// The rule it works by: a region of the base that only one side changed is
// taken from that side; a region both sides changed is not merged at all. A
// merge that is not obviously right is not done, because a wrongly merged file
// is worse than a conflict the user resolves by hand. Everything this package
// is unsure about is reported as an error, and the caller keeps both versions
// as it would have without it.
package textmerge

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"
)

// MaxSize is the largest version, in bytes, a merge is attempted on. It is a
// megabyte: a text file a line merge helps with is a document or source file,
// and past this size the input is either generated or not really text, while
// the cost of keeping a copy of the base for every such file is paid on every
// device. Anything larger keeps both versions.
const MaxSize = 1 << 20

// MaxEdits bounds how far the line diff of one side may be from the base,
// counted in inserted plus deleted lines after the common prefix and suffix
// are removed. Two versions further apart than this are a rewrite rather than
// an edit, and the merge gives up instead of spending time on a file it is
// unlikely to merge anyway.
const MaxEdits = 1000

// Why a merge was not done. Every one of these means the caller keeps both
// versions, which is what it would have done without this package at all.
var (
	// ErrNotText reports content that is not text: it holds a NUL byte or
	// it is not valid UTF-8. Binary content is never merged.
	ErrNotText = errors.New("content is not text")
	// ErrTooLarge reports a version above MaxSize.
	ErrTooLarge = errors.New("content is larger than the merge limit")
	// ErrOverlap reports that both sides changed the same region of the
	// base, so there is no one answer.
	ErrOverlap = errors.New("both sides changed the same lines")
	// ErrTooDifferent reports a side whose diff from the base is larger
	// than MaxEdits.
	ErrTooDifferent = errors.New("a side is too far from the base to merge")
)

// Merge returns the three-way merge of local and remote against the base they
// both descend from. A region of the base only one side touched is taken from
// that side; a region both sides touched, unless they changed it to exactly
// the same bytes, is an ErrOverlap and nothing is merged.
//
// Lines are compared whole, including their line terminator, and the merged
// file is built by concatenating them, so a file with CRLF terminators or
// without a final newline comes back byte for byte as its sides wrote it.
func Merge(base, local, remote []byte) ([]byte, error) {
	for _, side := range [][]byte{base, local, remote} {
		if len(side) > MaxSize {
			return nil, fmt.Errorf("%w of %d bytes", ErrTooLarge, MaxSize)
		}
		if !IsText(side) {
			return nil, ErrNotText
		}
	}
	switch {
	case bytes.Equal(local, remote):
		// Both sides made the same change, or neither made one.
		return local, nil
	case bytes.Equal(base, local):
		return remote, nil
	case bytes.Equal(base, remote):
		return local, nil
	}

	baseLines := lines(base)
	localHunks, err := hunks(baseLines, lines(local))
	if err != nil {
		return nil, err
	}
	remoteHunks, err := hunks(baseLines, lines(remote))
	if err != nil {
		return nil, err
	}
	return apply(baseLines, localHunks, remoteHunks)
}

// IsText reports whether b is content a line merge may look at: no NUL byte
// anywhere in it, and valid UTF-8 throughout. The whole input is checked
// rather than a prefix, because the input is bounded by MaxSize anyway and a
// file whose first kilobyte reads as text is exactly the file a prefix check
// would merge wrongly. Empty content is text.
func IsText(b []byte) bool {
	return bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}

// lines splits b into lines, each keeping its own terminator. Concatenating
// the result returns b exactly, so nothing about the file's line endings has
// to be guessed.
func lines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return append(out, b)
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// hunk is one change of a side against the base: the half-open range of base
// lines it replaces, and what it puts there. An empty range is an insertion
// before base line Start.
type hunk struct {
	Start, End int
	Lines      [][]byte
}

// hunks returns the changes that turn base into side, in base order and
// without touching regions between them.
func hunks(base, side [][]byte) ([]hunk, error) {
	// The common prefix and suffix are not part of any change, and taking
	// them off first is what keeps the diff of an ordinary edit small.
	lo := 0
	for lo < len(base) && lo < len(side) && bytes.Equal(base[lo], side[lo]) {
		lo++
	}
	hi := 0
	for hi < len(base)-lo && hi < len(side)-lo &&
		bytes.Equal(base[len(base)-1-hi], side[len(side)-1-hi]) {
		hi++
	}
	middleBase, middleSide := base[lo:len(base)-hi], side[lo:len(side)-hi]
	if len(middleBase) == 0 && len(middleSide) == 0 {
		return nil, nil
	}
	script, err := diff(middleBase, middleSide)
	if err != nil {
		return nil, err
	}
	out := make([]hunk, 0, len(script))
	for _, h := range script {
		out = append(out, hunk{Start: h.Start + lo, End: h.End + lo, Lines: h.Lines})
	}
	return out, nil
}

// diff returns the changes turning a into b, as hunks over a's indices. It is
// Myers' greedy algorithm: it walks the edit graph one edit distance at a
// time, so it stops as soon as it has found a shortest script, and it gives up
// at MaxEdits rather than running to completion on two unrelated files.
func diff(a, b [][]byte) ([]hunk, error) {
	n, m := len(a), len(b)
	maxD := n + m
	if maxD > MaxEdits {
		maxD = MaxEdits
	}
	// v holds, for every diagonal k, how far along a the furthest reaching
	// path of the current edit distance got. trace keeps one copy per
	// distance, which is what the path is walked back through.
	v := make([]int, 2*maxD+2)
	offset := maxD
	trace := make([][]int, 0, maxD+1)
	for d := 0; d <= maxD; d++ {
		snapshot := make([]int, len(v))
		copy(snapshot, v)
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			switch {
			case k == -d || (k != d && v[offset+k-1] < v[offset+k+1]):
				x = v[offset+k+1] // a line of b was inserted
			default:
				x = v[offset+k-1] + 1 // a line of a was deleted
			}
			y := x - k
			for x < n && y < m && bytes.Equal(a[x], b[y]) {
				x, y = x+1, y+1
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return walk(a, b, trace, offset, d), nil
			}
		}
	}
	return nil, fmt.Errorf("%w: more than %d line edits", ErrTooDifferent, MaxEdits)
}

// edit is one step of the shortest script: a base line removed, or a side line
// inserted before base line At.
type edit struct {
	At      int
	Line    []byte
	Deletes bool
}

// walk turns the trace of the edit graph back into the shortest edit script,
// in base order, and groups it into hunks. Two edits with no unchanged base
// line between them belong to the same hunk, which is what makes a replaced
// line one change rather than a deletion and an insertion.
func walk(a, b [][]byte, trace [][]int, offset, d int) []hunk {
	x, y := len(a), len(b)
	script := make([]edit, 0, d)
	for step := d; step > 0; step-- {
		v := trace[step]
		k := x - y
		var prevK int
		switch {
		case k == -step || (k != step && v[offset+k-1] < v[offset+k+1]):
			prevK = k + 1
		default:
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK

		// The diagonal run at the end of this step is base nobody
		// changed; it only moves the cursor.
		for x > prevX && y > prevY {
			x, y = x-1, y-1
		}
		if x > prevX {
			x--
			script = append(script, edit{At: x, Deletes: true})
		} else if y > prevY {
			y--
			script = append(script, edit{At: x, Line: b[y]})
		}
	}
	// The script was walked from the end, so it comes out reversed.
	for i, j := 0, len(script)-1; i < j; i, j = i+1, j-1 {
		script[i], script[j] = script[j], script[i]
	}
	return group(script)
}

// group collects a base-ordered edit script into hunks. A hunk runs until an
// edit that starts past its end, which is a base line the script left alone.
func group(script []edit) []hunk {
	var out []hunk
	for _, e := range script {
		if n := len(out); n > 0 && e.At <= out[n-1].End {
			if e.Deletes {
				out[n-1].End = e.At + 1
			} else {
				out[n-1].Lines = append(out[n-1].Lines, e.Line)
			}
			continue
		}
		h := hunk{Start: e.At, End: e.At}
		if e.Deletes {
			h.End = e.At + 1
		} else {
			h.Lines = [][]byte{e.Line}
		}
		out = append(out, h)
	}
	return out
}

// apply builds the merged file from the base and the two change lists. A base
// region both sides changed, to anything but the same bytes, is refused: the
// two hunks have to be separated by at least one base line neither touched for
// the result to have one obvious reading.
func apply(base [][]byte, local, remote []hunk) ([]byte, error) {
	merged := make([][]byte, 0, len(base))
	at := 0 // the next base line to copy
	i, j := 0, 0
	for i < len(local) || j < len(remote) {
		switch {
		case j == len(remote) || (i < len(local) && local[i].Start <= remote[j].Start):
			h := local[i]
			if j < len(remote) && overlap(h, remote[j]) {
				if !sameChange(h, remote[j]) {
					return nil, fmt.Errorf("%w: base lines %d to %d",
						ErrOverlap, h.Start+1, max(h.End, remote[j].End))
				}
				// Both sides made the identical change, so it is
				// applied once and the peer's copy is dropped.
				j++
			}
			at = emit(&merged, base, at, h)
			i++
		default:
			h := remote[j]
			if i < len(local) && overlap(h, local[i]) {
				if !sameChange(h, local[i]) {
					return nil, fmt.Errorf("%w: base lines %d to %d",
						ErrOverlap, h.Start+1, max(h.End, local[i].End))
				}
				i++
			}
			at = emit(&merged, base, at, h)
			j++
		}
	}
	merged = append(merged, base[at:]...)
	return bytes.Join(merged, nil), nil
}

// emit copies the untouched base lines up to h and then h's replacement,
// returning the next base line to copy.
func emit(merged *[][]byte, base [][]byte, at int, h hunk) int {
	if h.Start > at {
		*merged = append(*merged, base[at:h.Start]...)
	}
	*merged = append(*merged, h.Lines...)
	return max(at, h.End)
}

// overlap reports whether two hunks leave no base line between them. Touching
// counts as overlapping: an insertion at the same place as another change has
// no one order, and a merge with no one reading is not made.
func overlap(a, b hunk) bool { return a.End >= b.Start && b.End >= a.Start }

// sameChange reports whether two hunks replace the same base lines by the same
// content, which is both sides having made one edit rather than two.
func sameChange(a, b hunk) bool {
	if a.Start != b.Start || a.End != b.End || len(a.Lines) != len(b.Lines) {
		return false
	}
	for i := range a.Lines {
		if !bytes.Equal(a.Lines[i], b.Lines[i]) {
			return false
		}
	}
	return true
}
