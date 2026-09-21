package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// pausedView is the JSON shape the pause and resume commands print.
type pausedView struct {
	Folders []string `json:"folders"`
	Paused  bool     `json:"paused"`
}

// newPauseCmd builds the "pause" command.
func newPauseCmd(deps *Deps) *cobra.Command {
	return newSetPausedCmd(deps, true, "pause", "Stop scanning and syncing a folder")
}

// newResumeCmd builds the "resume" command.
func newResumeCmd(deps *Deps) *cobra.Command {
	return newSetPausedCmd(deps, false, "resume", "Resume a paused folder")
}

// newSetPausedCmd builds a command that flips the paused flag of one folder,
// or of every folder when no id is given. A running daemon does the work so
// that it notices immediately; otherwise the database is written directly.
func newSetPausedCmd(deps *Deps, paused bool, name, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [FOLDER]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			targets, err := folderIDs(cmd.Context(), app, args)
			if err != nil {
				return err
			}
			var id folder.ID
			if len(args) == 1 {
				id = folder.ID(args[0])
			}

			if err := setPaused(cmd.Context(), app, id, paused); err != nil {
				return err
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(pausedView{Folders: idStrings(targets), Paused: paused})
			}
			if len(targets) == 0 {
				_, err := fmt.Fprintln(deps.Stdout, "no folders configured")
				return err
			}
			for _, target := range targets {
				if _, err := fmt.Fprintf(deps.Stdout, "%s: %sd\n", target, name); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// setPaused asks the daemon to flip the flag, falling back to a direct write
// when none is running.
func setPaused(ctx context.Context, app *compose.App, id folder.ID, paused bool) error {
	var err error
	if paused {
		err = app.Control.Pause(ctx, id)
	} else {
		err = app.Control.Resume(ctx, id)
	}
	if !errors.Is(err, ports.ErrDaemonUnavailable) {
		return err
	}

	targets, err := folderIDs(ctx, app, argsOf(id))
	if err != nil {
		return err
	}
	for _, target := range targets {
		if err := app.Folders.SetPaused(ctx, target, paused); err != nil {
			return err
		}
	}
	return nil
}

// folderIDs resolves the folders a command applies to: the one named by the
// argument, or every configured folder.
func folderIDs(ctx context.Context, app *compose.App, args []string) ([]folder.ID, error) {
	if len(args) == 1 {
		f, err := app.Folders.Get(ctx, folder.ID(args[0]))
		if err != nil {
			return nil, err
		}
		return []folder.ID{f.ID}, nil
	}

	all, err := app.Folders.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]folder.ID, 0, len(all))
	for _, f := range all {
		out = append(out, f.ID)
	}
	return out, nil
}

// argsOf turns an optional folder id back into command arguments.
func argsOf(id folder.ID) []string {
	if id == "" {
		return nil
	}
	return []string{string(id)}
}

// idStrings converts folder ids to strings for JSON output.
func idStrings(ids []folder.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}
