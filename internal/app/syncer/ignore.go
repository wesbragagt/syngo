package syncer

import (
	"context"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/scan"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/ignore"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
	"github.com/wesbragagt/syngo/internal/domain/syncplan"
)

// ReasonIgnored explains a plan item dropped because the folder's ignore rules
// cover its path.
const ReasonIgnored = "ignored"

// loadIgnore builds the ignore rules of a folder: the configured patterns plus
// the lines of <root>/.syngoignore, the same rules a scan applies. They hold
// for what a peer offers as much as for what a scan finds, otherwise an
// ignored path would arrive from the peer, be installed, and then be read as
// deleted by the next scan and the deletion sent back.
func loadIgnore(ctx context.Context, fsys ports.FileSystem, f folder.Folder) (*ignore.Matcher, error) {
	return scan.LoadIgnore(ctx, fsys, f)
}

// dropIgnored returns the items whose path the folder does not ignore, and the
// paths that were dropped.
func dropIgnored(m *ignore.Matcher, items []syncplan.Item) (kept []syncplan.Item, ignored []string) {
	for _, item := range items {
		isDir := item.Remote != nil && item.Remote.Kind == manifest.KindDir
		if m.Match(item.Path, isDir) {
			ignored = append(ignored, item.Path)
			continue
		}
		kept = append(kept, item)
	}
	return kept, ignored
}
