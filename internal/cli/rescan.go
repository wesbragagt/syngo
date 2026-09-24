package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/scan"
	"github.com/wesbragagt/syngo/internal/compose"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// newRescanCmd builds the "rescan" command.
func newRescanCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "rescan [ID]",
		Short: "Rescan one folder, or every folder that is not paused",
		Args:  atMostArgs(1, "expected at most one folder ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			targets, err := scanTargets(cmd.Context(), app, args)
			if err != nil {
				return err
			}

			// A running daemon owns the folders; it queues the scan so
			// two processes never write the same folder at once.
			queued, err := queueRescan(cmd.Context(), app, args, targets)
			if err != nil {
				return err
			}
			if queued != nil {
				return printQueued(deps, queued)
			}

			views := make([]scanView, 0, len(targets))
			for _, f := range targets {
				result, err := app.Scan.Scan(cmd.Context(), f, scan.Options{})
				if err != nil {
					return err
				}
				views = append(views, viewOfResult(result))
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(views)
			}
			for _, v := range views {
				if _, err := fmt.Fprintf(deps.Stdout, "%s: %d change(s) (scanned %d, hashed %d, ignored %d)\n",
					v.FolderID, v.Summary.Changes, v.Summary.Scanned, v.Summary.Hashed, v.Summary.Ignored); err != nil {
					return err
				}
			}
			if len(views) == 0 {
				_, err := fmt.Fprintln(deps.Stdout, "no folders to scan")
				return err
			}
			return nil
		},
	}
}

// queuedView is the JSON shape rescan prints when a daemon does the work.
type queuedView struct {
	Queued []string `json:"queued"`
}

// queueRescan asks a running daemon to rescan the targets. It returns nil
// when no daemon is running and the caller has to scan by itself.
func queueRescan(ctx context.Context, app *compose.App, args []string, targets []folder.Folder) ([]string, error) {
	var id folder.ID
	if len(args) == 1 {
		id = folder.ID(args[0])
	}
	err := app.Control.Rescan(ctx, id)
	switch {
	case errors.Is(err, ports.ErrDaemonUnavailable):
		return nil, nil
	case err != nil:
		return nil, err
	}

	queued := make([]string, 0, len(targets))
	for _, f := range targets {
		queued = append(queued, string(f.ID))
	}
	return queued, nil
}

// printQueued reports the folders whose rescan the daemon accepted.
func printQueued(deps *Deps, queued []string) error {
	if deps.Global.JSON {
		return deps.printer().PrintJSON(queuedView{Queued: queued})
	}
	if len(queued) == 0 {
		_, err := fmt.Fprintln(deps.Stdout, "no folders to scan")
		return err
	}
	for _, id := range queued {
		if _, err := fmt.Fprintf(deps.Stdout, "%s: rescan queued\n", id); err != nil {
			return err
		}
	}
	return nil
}

// scanTargets resolves the folders a rescan or repair run applies to. Without
// an argument it returns every folder that is not paused.
func scanTargets(ctx context.Context, app *compose.App, args []string) ([]folder.Folder, error) {
	if len(args) == 1 {
		f, err := app.Folders.Get(ctx, folder.ID(args[0]))
		if err != nil {
			return nil, err
		}
		return []folder.Folder{f}, nil
	}

	all, err := app.Folders.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]folder.Folder, 0, len(all))
	for _, f := range all {
		if !f.Paused {
			out = append(out, f)
		}
	}
	return out, nil
}
