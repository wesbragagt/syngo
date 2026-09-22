package e2e

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// The document the two devices share and the two edits they make to it while
// they cannot see each other. The first edit changes the opening line and the
// second the closing one, with four untouched lines between them, so a
// three-way merge has one obvious answer.
const (
	sharedDoc = "title\nalpha\nbeta\ngamma\ndelta\nepsilon\n"
	editOnA   = "TITLE\nalpha\nbeta\ngamma\ndelta\nepsilon\n"
	editOnB   = "title\nalpha\nbeta\ngamma\ndelta\nEPSILON\n"
	mergedDoc = "TITLE\nalpha\nbeta\ngamma\ndelta\nEPSILON\n"
)

// divergeText makes both nodes edit the same text file to the given contents
// while they cannot reach each other, then lets them meet again. It is
// divergeOn with the contents chosen by the caller.
func divergeText(t *testing.T, textMerge bool, fromA, fromB string) (a, b *node) {
	t.Helper()
	a = startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, TextMerge: textMerge})
	b = startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive, TextMerge: textMerge})
	writeFile(t, a, "doc.txt", []byte(sharedDoc))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	pauseFolder(t, b)
	writeFile(t, a, "doc.txt", []byte(fromA))
	writeFile(t, b, "doc.txt", []byte(fromB))
	resumeFolder(t, b)
	return a, b
}

// TestTextMerge_DisjointEditsMergeWithoutAConflict is the case the setting
// exists for: two devices edited different lines of one document while apart,
// and both edits are in the file afterwards with nothing left to decide.
func TestTextMerge_DisjointEditsMergeWithoutAConflict(t *testing.T) {
	a, b := divergeText(t, true, editOnA, editOnB)
	waitFileContent(t, a, "doc.txt", mergedDoc)
	waitFileContent(t, b, "doc.txt", mergedDoc)
	waitConverged(t, a, b)

	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if len(files) != 1 {
			t.Fatalf("%s holds %v, want the merged file alone", n.name, files)
		}
		if files["doc.txt"] != mergedDoc {
			t.Fatalf("%s: doc.txt = %q, want %q", n.name, files["doc.txt"], mergedDoc)
		}
		if copies := copiesIn(files); len(copies) != 0 {
			t.Fatalf("%s: conflict copies = %v, want none", n.name, copies)
		}
		if rows := conflictsOf(t, n); len(rows) != 0 {
			t.Fatalf("%s reports %+v, want no conflict record", n.name, rows)
		}
		if report := n.Status(); report.Conflicts.Unresolved != 0 {
			t.Fatalf("%s reports %d unresolved conflicts, want 0",
				n.name, report.Conflicts.Unresolved)
		}
	}
	// Exactly one device merges: the merged file dominates both versions,
	// so the other takes it as an ordinary newer version.
	if merges := a.Events.Count("sync.merged") + b.Events.Count("sync.merged"); merges != 1 {
		t.Fatalf("sync.merged events = %d across both devices, want 1", merges)
	}
}

// TestTextMerge_TheSameLineOnBothSidesStillKeepsBoth: the setting changes
// nothing about the conflict that has no answer.
func TestTextMerge_TheSameLineOnBothSidesStillKeepsBoth(t *testing.T) {
	sameLineA := "title\nalpha\nCHANGED ON A\ngamma\ndelta\nepsilon\n"
	sameLineB := "title\nalpha\nCHANGED ON B\ngamma\ndelta\nepsilon\n"
	a, b := divergeText(t, true, sameLineA, sameLineB)
	waitConverged(t, a, b)

	kept, moved := sameLineA, sameLineB
	loser := b
	if !keepsPath(sameLineA, sameLineB) {
		kept, moved, loser = sameLineB, sameLineA, a
	}
	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if files["doc.txt"] != kept {
			t.Fatalf("%s: doc.txt = %q, want %q", n.name, files["doc.txt"], kept)
		}
		copies := copiesIn(files)
		if len(copies) != 1 || files[copies[0]] != moved {
			t.Fatalf("%s: copies = %v of %v, want one holding %q", n.name, copies, files, moved)
		}
	}
	rows := conflictsOf(t, loser)
	if len(rows) != 1 || rows[0].Path != "doc.txt" || rows[0].Reason != "both-modified" {
		t.Fatalf("%s reports %+v, want one both-modified conflict", loser.name, rows)
	}
	if merges := a.Events.Count("sync.merged") + b.Events.Count("sync.merged"); merges != 0 {
		t.Fatalf("sync.merged events = %d, want 0: the edits overlapped", merges)
	}
}

// TestTextMerge_OffByDefaultKeepsBoth proves the default explicitly: the very
// edits that merge with the setting on produce the keep-both conflict without
// it, so nothing about an existing installation changes.
func TestTextMerge_OffByDefaultKeepsBoth(t *testing.T) {
	a, b := divergeText(t, false, editOnA, editOnB)
	waitConverged(t, a, b)

	kept, moved := editOnA, editOnB
	loser := b
	if !keepsPath(editOnA, editOnB) {
		kept, moved, loser = editOnB, editOnA, a
	}
	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if files["doc.txt"] != kept {
			t.Fatalf("%s: doc.txt = %q, want the keep-both winner %q", n.name, files["doc.txt"], kept)
		}
		if holds(files, mergedDoc) {
			t.Fatalf("%s merged the edits with the setting off: %v", n.name, files)
		}
		copies := copiesIn(files)
		if len(copies) != 1 || files[copies[0]] != moved {
			t.Fatalf("%s: copies = %v of %v, want one holding %q", n.name, copies, files, moved)
		}
	}
	if rows := conflictsOf(t, loser); len(rows) != 1 {
		t.Fatalf("%s reports %+v, want one conflict", loser.name, rows)
	}
	if merges := a.Events.Count("sync.merged") + b.Events.Count("sync.merged"); merges != 0 {
		t.Fatalf("sync.merged events = %d, want 0: the setting is off", merges)
	}
}

// TestTextMerge_BinaryContentIsNeverMerged: the same divergence on a file
// holding a NUL byte keeps both versions even with the setting on, which is
// the outcome an automatic merge must never replace.
func TestTextMerge_BinaryContentIsNeverMerged(t *testing.T) {
	binaryA := "head\x00\nEDITED ON A\nmiddle\nbody\ntail\n"
	binaryB := "head\x00\nfirst\nmiddle\nbody\nEDITED ON B\n"
	a := startNodeWith(t, "a", nodeConfig{Mode: folder.ModeSendReceive, TextMerge: true})
	b := startNodeWith(t, "b", nodeConfig{Mode: folder.ModeSendReceive, TextMerge: true})
	writeFile(t, a, "doc.bin", []byte("head\x00\nfirst\nmiddle\nbody\ntail\n"))
	pair(t, a, b)
	waitTreeEqual(t, a, b)

	pauseFolder(t, b)
	writeFile(t, a, "doc.bin", []byte(binaryA))
	writeFile(t, b, "doc.bin", []byte(binaryB))
	resumeFolder(t, b)
	waitConverged(t, a, b)

	kept, moved := binaryA, binaryB
	if !keepsPath(binaryA, binaryB) {
		kept, moved = binaryB, binaryA
	}
	for _, n := range []*node{a, b} {
		files := contentsOf(t, n)
		if files["doc.bin"] != kept {
			t.Fatalf("%s: doc.bin = %q, want %q", n.name, files["doc.bin"], kept)
		}
		copies := copiesIn(files)
		if len(copies) != 1 || files[copies[0]] != moved {
			t.Fatalf("%s: copies = %v of %v, want one holding the other version", n.name, copies, files)
		}
	}
	if merges := a.Events.Count("sync.merged") + b.Events.Count("sync.merged"); merges != 0 {
		t.Fatalf("sync.merged events = %d, want 0: the file is not text", merges)
	}
}
