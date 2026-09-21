package manifest

import (
	"sort"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// Manifest is a snapshot of a folder's contents, including tombstones.
type Manifest struct {
	FolderID folder.ID
	Sequence uint64
	Entries  map[string]Entry
}

// New returns an empty manifest for the given folder.
func New(folderID folder.ID) Manifest {
	return Manifest{FolderID: folderID, Entries: make(map[string]Entry)}
}

// Add inserts or replaces an entry, keyed by its path.
func (m Manifest) Add(e Entry) {
	m.Entries[e.Path] = e
}

// Sorted returns every entry, including tombstones, ordered by path.
func (m Manifest) Sorted() []Entry {
	out := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Live returns every entry that is not a tombstone, ordered by path.
func (m Manifest) Live() []Entry {
	all := m.Sorted()
	out := all[:0]
	for _, e := range all {
		if !e.Deleted {
			out = append(out, e)
		}
	}
	return out
}
