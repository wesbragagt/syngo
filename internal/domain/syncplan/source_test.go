package syncplan_test

import (
	"testing"

	"github.com/wesbragagt/syncthis/internal/domain/manifest"
	"github.com/wesbragagt/syncthis/internal/domain/syncplan"
)

// pullOf returns a pull item for a remote entry.
func pullOf(e manifest.Entry) syncplan.Item {
	return syncplan.Item{Action: syncplan.ActionPull, Path: e.Path, Remote: &e,
		Reason: syncplan.ReasonMissing}
}

func TestLocalSources_Rules(t *testing.T) {
	moved := file("new.bin", "v1", vec("A:2"))
	tests := []struct {
		name  string
		local manifest.Manifest
		items []syncplan.Item
		want  map[string]string
	}{
		{
			name:  "a path the peer renamed is taken from the old one",
			local: manifestOf(file("old.bin", "v1", vec("A:1"))),
			items: []syncplan.Item{pullOf(moved)},
			want:  map[string]string{"new.bin": "old.bin"},
		},
		{
			name:  "content nothing local holds is downloaded",
			local: manifestOf(file("other.bin", "v9", vec("A:1"))),
			items: []syncplan.Item{pullOf(moved)},
			want:  nil,
		},
		{
			name:  "a tombstone is not a source",
			local: manifestOf(tombstone("old.bin", vec("A:1"))),
			items: []syncplan.Item{pullOf(moved)},
			want:  nil,
		},
		{
			name: "the lowest of several holders is chosen, so two runs agree",
			local: manifestOf(
				file("z.bin", "v1", vec("A:1")),
				file("a.bin", "v1", vec("A:1")),
			),
			items: []syncplan.Item{pullOf(moved)},
			want:  map[string]string{"new.bin": "a.bin"},
		},
		{
			name:  "a delete is never satisfied from a local file",
			local: manifestOf(file("old.bin", "v1", vec("A:1"))),
			items: []syncplan.Item{{Action: syncplan.ActionDelete, Path: "new.bin", Remote: &moved}},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := syncplan.LocalSources(tc.local, tc.items)
			if len(got) != len(tc.want) {
				t.Fatalf("sources = %v, want %v", got, tc.want)
			}
			for path, want := range tc.want {
				if got[path] != want {
					t.Fatalf("source of %s = %q, want %q", path, got[path], want)
				}
			}
		})
	}
}

// TestLocalSources_EmptyAndHashlessEntriesAreNotSources pins the entries that
// never take part: an empty file says nothing about content, and an entry
// without a hash was never read.
func TestLocalSources_EmptyAndHashlessEntriesAreNotSources(t *testing.T) {
	empty := manifest.Entry{Path: "empty.bin", Kind: manifest.KindFile, Size: 0, Hash: hash("v1")}
	hashless := manifest.Entry{Path: "raw.bin", Kind: manifest.KindFile, Size: 1}

	local := manifestOf(
		manifest.Entry{Path: "other-empty.bin", Kind: manifest.KindFile, Size: 0, Hash: hash("v1")},
		manifest.Entry{Path: "other-raw.bin", Kind: manifest.KindFile, Size: 1},
	)
	if got := syncplan.LocalSources(local, []syncplan.Item{pullOf(empty), pullOf(hashless)}); len(got) != 0 {
		t.Fatalf("sources = %v, want none", got)
	}
}

// TestLocalSources_TheSamePathIsNotItsOwnSource covers the pull of a path that
// is already recorded with that content, which copying would only undo.
func TestLocalSources_TheSamePathIsNotItsOwnSource(t *testing.T) {
	same := file("a.bin", "v1", vec("A:2"))
	local := manifestOf(file("a.bin", "v1", vec("A:1")))
	if got := syncplan.LocalSources(local, []syncplan.Item{pullOf(same)}); len(got) != 0 {
		t.Fatalf("sources = %v, want none", got)
	}
}
