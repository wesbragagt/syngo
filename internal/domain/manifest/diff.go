package manifest

import "sort"

// ChangeType classifies a change between two manifests.
type ChangeType uint8

// Change types.
const (
	Create ChangeType = iota + 1
	Modify
	Delete
	Rename
)

// String returns the lowercase name of the change type, matching the value
// stored in the changes table.
func (t ChangeType) String() string {
	switch t {
	case Create:
		return "create"
	case Modify:
		return "modify"
	case Delete:
		return "delete"
	case Rename:
		return "rename"
	default:
		return "unknown"
	}
}

// Change is a single difference between two manifests. Path is the current
// path; OldPath is set for renames only. Old is nil for creates and New is nil
// for deletes.
type Change struct {
	Type    ChangeType
	Path    string
	OldPath string
	Old     *Entry
	New     *Entry
}

// Diff reports how cur differs from old, ordered by Path.
//
// An entry present and live in cur but absent or a tombstone in old is a
// Create. An entry live in both whose Kind, Size, Hash, LinkTarget or Mode
// differs is a Modify. An entry live in old that is absent or a tombstone in
// cur is a Delete. Finally a rename pass pairs a Delete with a Create when both
// are non-empty files with the same hash and size, and that (hash, size) pair
// has exactly one Delete and one Create.
func Diff(old, cur Manifest) []Change {
	var changes []Change

	for _, curEntry := range cur.Sorted() {
		oldEntry, inOld := old.Entries[curEntry.Path]
		switch {
		case curEntry.Deleted:
			if inOld && !oldEntry.Deleted {
				changes = append(changes, Change{Type: Delete, Path: curEntry.Path, Old: entryPtr(oldEntry)})
			}
		case !inOld || oldEntry.Deleted:
			changes = append(changes, Change{Type: Create, Path: curEntry.Path, New: entryPtr(curEntry)})
		case differs(oldEntry, curEntry):
			changes = append(changes, Change{
				Type: Modify,
				Path: curEntry.Path,
				Old:  entryPtr(oldEntry),
				New:  entryPtr(curEntry),
			})
		}
	}

	for _, oldEntry := range old.Sorted() {
		if oldEntry.Deleted {
			continue
		}
		if _, inCur := cur.Entries[oldEntry.Path]; inCur {
			continue
		}
		changes = append(changes, Change{Type: Delete, Path: oldEntry.Path, Old: entryPtr(oldEntry)})
	}

	changes = detectRenames(changes)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// differs reports whether two live entries describe different content.
func differs(a, b Entry) bool {
	return a.Kind != b.Kind ||
		a.Size != b.Size ||
		a.Hash != b.Hash ||
		a.LinkTarget != b.LinkTarget ||
		a.Mode != b.Mode
}

// ContentDiffers reports whether two live entries differ in what a version
// vector counts: the kind, the content hash and the link target. The
// permission bits are not content, so a chmod is a Modify that carries the
// version of the entry it changed.
func ContentDiffers(a, b Entry) bool {
	return a.Kind != b.Kind || a.Hash != b.Hash || a.LinkTarget != b.LinkTarget
}

// renameKey groups candidates by identical content.
type renameKey struct {
	hash Hash
	size int64
}

// detectRenames collapses unambiguous delete/create pairs of identical file
// content into a single Rename change.
func detectRenames(changes []Change) []Change {
	deletes := make(map[renameKey][]int)
	creates := make(map[renameKey][]int)

	for i, c := range changes {
		switch c.Type {
		case Delete:
			if e := c.Old; renameCandidate(e) {
				deletes[renameKey{e.Hash, e.Size}] = append(deletes[renameKey{e.Hash, e.Size}], i)
			}
		case Create:
			if e := c.New; renameCandidate(e) {
				creates[renameKey{e.Hash, e.Size}] = append(creates[renameKey{e.Hash, e.Size}], i)
			}
		}
	}

	dropped := make(map[int]bool)
	for key, delIdx := range deletes {
		crIdx := creates[key]
		if len(delIdx) != 1 || len(crIdx) != 1 {
			continue
		}
		del, cr := changes[delIdx[0]], changes[crIdx[0]]
		changes[crIdx[0]] = Change{
			Type:    Rename,
			Path:    cr.Path,
			OldPath: del.Path,
			Old:     del.Old,
			New:     cr.New,
		}
		dropped[delIdx[0]] = true
	}
	if len(dropped) == 0 {
		return changes
	}

	out := make([]Change, 0, len(changes)-len(dropped))
	for i, c := range changes {
		if !dropped[i] {
			out = append(out, c)
		}
	}
	return out
}

// renameCandidate reports whether an entry can take part in rename detection.
func renameCandidate(e *Entry) bool {
	return e != nil && e.Kind == KindFile && e.Size > 0 && !e.Hash.IsZero()
}

// entryPtr copies e to the heap so Change can hold a stable pointer.
func entryPtr(e Entry) *Entry {
	cp := e
	return &cp
}
