package manifest_test

import (
	"testing"

	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// hashOf builds a deterministic non-zero hash from a seed byte.
func hashOf(seed byte) manifest.Hash {
	var h manifest.Hash
	for i := range h {
		h[i] = seed
	}
	return h
}

func file(path string, seed byte, size int64) manifest.Entry {
	return manifest.Entry{Path: path, Kind: manifest.KindFile, Size: size, Hash: hashOf(seed), Mode: 0o644}
}

func manifestOf(entries ...manifest.Entry) manifest.Manifest {
	m := manifest.New("docs")
	for _, e := range entries {
		m.Add(e)
	}
	return m
}

func types(changes []manifest.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Type.String()+":"+c.Path)
	}
	return out
}

func TestDiff_Create(t *testing.T) {
	changes := manifest.Diff(manifestOf(), manifestOf(file("a.txt", 1, 10)))

	if len(changes) != 1 || changes[0].Type != manifest.Create {
		t.Fatalf("changes = %v, want one create", types(changes))
	}
	if changes[0].Old != nil || changes[0].New == nil {
		t.Fatalf("create should carry New only, got Old=%v New=%v", changes[0].Old, changes[0].New)
	}
}

func TestDiff_Modify(t *testing.T) {
	tests := []struct {
		name string
		cur  manifest.Entry
		want bool
	}{
		{"hash changed", file("a.txt", 2, 10), true},
		{"size changed", file("a.txt", 1, 11), true},
		{"kind changed", manifest.Entry{Path: "a.txt", Kind: manifest.KindDir, Mode: 0o644}, true},
		{"mode changed", manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Size: 10, Hash: hashOf(1), Mode: 0o600}, true},
		{"link target changed", manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Size: 10, Hash: hashOf(1), Mode: 0o644, LinkTarget: "b"}, true},
		{"mtime only", manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Size: 10, Hash: hashOf(1), Mode: 0o644, Inode: 99}, false},
	}
	old := manifestOf(file("a.txt", 1, 10))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := manifest.Diff(old, manifestOf(tt.cur))
			if !tt.want {
				if len(changes) != 0 {
					t.Fatalf("changes = %v, want none", types(changes))
				}
				return
			}
			if len(changes) != 1 || changes[0].Type != manifest.Modify {
				t.Fatalf("changes = %v, want one modify", types(changes))
			}
			if changes[0].Old == nil || changes[0].New == nil {
				t.Fatal("modify should carry both Old and New")
			}
		})
	}
}

func TestDiff_Delete(t *testing.T) {
	old := manifestOf(file("a.txt", 1, 10))

	t.Run("absent in cur", func(t *testing.T) {
		changes := manifest.Diff(old, manifestOf())
		if len(changes) != 1 || changes[0].Type != manifest.Delete || changes[0].New != nil {
			t.Fatalf("changes = %v, want one delete", types(changes))
		}
	})

	t.Run("tombstone in cur", func(t *testing.T) {
		tomb := file("a.txt", 1, 10)
		tomb.Deleted = true
		changes := manifest.Diff(old, manifestOf(tomb))
		if len(changes) != 1 || changes[0].Type != manifest.Delete {
			t.Fatalf("changes = %v, want one delete", types(changes))
		}
	})

	t.Run("tombstone in both", func(t *testing.T) {
		tomb := file("a.txt", 1, 10)
		tomb.Deleted = true
		changes := manifest.Diff(manifestOf(tomb), manifestOf(tomb))
		if len(changes) != 0 {
			t.Fatalf("changes = %v, want none", types(changes))
		}
	})
}

func TestDiff_RenameSingle(t *testing.T) {
	changes := manifest.Diff(manifestOf(file("a.txt", 1, 10)), manifestOf(file("b.txt", 1, 10)))

	if len(changes) != 1 {
		t.Fatalf("changes = %v, want one rename", types(changes))
	}
	c := changes[0]
	if c.Type != manifest.Rename || c.Path != "b.txt" || c.OldPath != "a.txt" {
		t.Fatalf("got %v %q <- %q, want rename b.txt <- a.txt", c.Type, c.Path, c.OldPath)
	}
	if c.Old == nil || c.New == nil {
		t.Fatal("rename should carry both Old and New")
	}
}

func TestDiff_RenameAmbiguousFallsBackToDeleteCreate(t *testing.T) {
	old := manifestOf(file("a.txt", 1, 10), file("b.txt", 1, 10))
	cur := manifestOf(file("c.txt", 1, 10))

	changes := manifest.Diff(old, cur)

	if len(changes) != 3 {
		t.Fatalf("changes = %v, want two deletes and one create", types(changes))
	}
	for _, c := range changes {
		if c.Type == manifest.Rename {
			t.Fatalf("changes = %v, want no rename", types(changes))
		}
	}
}

func TestDiff_EmptyFilesNeverRename(t *testing.T) {
	old := manifestOf(manifest.Entry{Path: "a.txt", Kind: manifest.KindFile})
	cur := manifestOf(manifest.Entry{Path: "b.txt", Kind: manifest.KindFile})

	changes := manifest.Diff(old, cur)

	want := []string{"delete:a.txt", "create:b.txt"}
	got := types(changes)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

func TestDiff_TombstoneThenRecreateIsCreate(t *testing.T) {
	tomb := manifest.Entry{Path: "a.txt", Kind: manifest.KindFile, Deleted: true}
	changes := manifest.Diff(manifestOf(tomb), manifestOf(file("a.txt", 1, 10)))

	if len(changes) != 1 || changes[0].Type != manifest.Create {
		t.Fatalf("changes = %v, want one create", types(changes))
	}
}

func TestDiff_DeterministicOrder(t *testing.T) {
	old := manifestOf(file("keep.txt", 9, 5), file("gone.txt", 3, 7), file("moved.txt", 4, 8))
	cur := manifestOf(
		file("keep.txt", 8, 5),
		file("new.txt", 5, 4),
		file("zz.txt", 4, 8),
		file("aa.txt", 6, 4),
	)

	want := []string{"create:aa.txt", "delete:gone.txt", "modify:keep.txt", "create:new.txt", "rename:zz.txt"}
	for i := 0; i < 20; i++ {
		got := types(manifest.Diff(old, cur))
		if len(got) != len(want) {
			t.Fatalf("changes = %v, want %v", got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: changes = %v, want %v", i, got, want)
			}
		}
	}
}
