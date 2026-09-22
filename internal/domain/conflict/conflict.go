// Package conflict names the copies a keep-both resolution writes and decides,
// from the content alone, which of two divergent versions keeps the path.
//
// Both sides of a conflict run the same rules on the same two hashes, so they
// reach the same answer without asking each other: exactly one side moves its
// version aside, and the copy it writes reaches the other side as an ordinary
// file. That is what makes two divergent trees converge again instead of
// swapping versions forever.
package conflict

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// Marker is what every conflict copy carries in its name. It is the only way
// a copy can be told apart from any other file, on this device or on a peer.
const Marker = ".sync-conflict-"

// timeLayout is the date and time a conflict name embeds, in UTC.
const timeLayout = "20060102-150405"

// Name returns the name of the conflict copy of rel: the stem, the marker, the
// modification time of the version being moved aside, the short id of the
// device that wrote it, and the original extension. The directory part of rel
// is kept, so the copy sits next to the file it belongs to.
//
// The name describes the version, not the meeting of two devices that noticed
// it. Every device that moves that version aside therefore writes the same
// name, and the copies converge into one file instead of one per device pair.
//
//	"notes/report.md" -> "notes/report.sync-conflict-20260921-153000-ABCDEFG.md"
func Name(rel string, at time.Time, by device.ID) string {
	dir, base := path.Split(rel)
	stem, ext := split(base)
	name := stem + Marker + at.UTC().Format(timeLayout) + "-" + by.Short() + ext
	return dir + name
}

// WithCounter returns a variant of a conflict name for the n-th attempt, used
// when the name is already taken. The counter goes before the extension, so
// the copy keeps opening in the same application as the original.
//
//	("report.sync-conflict-…-ABCDEFG.md", 2) -> "report.sync-conflict-…-ABCDEFG-2.md"
func WithCounter(name string, n int) string {
	dir, base := path.Split(name)
	stem, ext := split(base)
	return dir + stem + "-" + strconv.Itoa(n) + ext
}

// IsCopy reports whether rel is the name of a conflict copy.
func IsCopy(rel string) bool {
	return strings.Contains(path.Base(rel), Marker)
}

// KeepsPath reports whether the local version of a path keeps it, leaving the
// peer's version to be stored under a conflict name on the peer's side. The
// rule is a total order on the content: the greater hash keeps the path. It
// depends on nothing but the two versions, so every device involved reaches
// the same answer, and a third device joining later reaches it too.
func KeepsPath(local, remote manifest.Hash) bool {
	for i := range local {
		if local[i] != remote[i] {
			return local[i] > remote[i]
		}
	}
	// Identical content is not a conflict; keeping the local version is the
	// answer that changes nothing.
	return true
}

// RecordID returns the identifier of a conflict. It is derived from what the
// conflict is, not from when it was noticed, so a divergence that is reported
// once per sync cycle updates one row instead of piling up new ones.
func RecordID(id folder.ID, rel string, local, remote manifest.Hash, peer device.ID) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{string(id), rel, local.String(), remote.String(), string(peer)}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// split separates the extension from the rest of a file name. A name that is
// nothing but an extension, such as ".bashrc", has none: the dot starts the
// name rather than an extension.
func split(base string) (stem, ext string) {
	ext = path.Ext(base)
	if ext == base {
		return base, ""
	}
	return strings.TrimSuffix(base, ext), ext
}
