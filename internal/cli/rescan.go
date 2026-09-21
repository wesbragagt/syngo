package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// newRescanCmd builds the "rescan" command.
func newRescanCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "rescan [ID]",
		Short: "Rescan one folder, or every folder that is not paused",
		Args:  cobra.MaximumNArgs(1),
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
