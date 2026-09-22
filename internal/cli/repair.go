package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/repair"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// findingView is the JSON shape of a problem repair could not fix.
type findingView struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// repairView is the JSON shape of one repaired folder.
type repairView struct {
	FolderID        string        `json:"folder_id"`
	DryRun          bool          `json:"dry_run"`
	MissingDeleted  []string      `json:"missing_deleted"`
	Rehashed        []string      `json:"rehashed"`
	TransfersReset  []string      `json:"transfers_reset"`
	TransfersPruned []string      `json:"transfers_pruned"`
	OrphanTemps     []string      `json:"orphan_temps"`
	SequenceFixed   bool          `json:"sequence_fixed"`
	Unrecoverable   []findingView `json:"unrecoverable"`
}

// viewOfRepair converts a repair report into its JSON shape.
func viewOfRepair(r repair.Report) repairView {
	findings := make([]findingView, 0, len(r.Unrecoverable))
	for _, f := range r.Unrecoverable {
		findings = append(findings, findingView{Path: f.Path, Reason: f.Reason})
	}
	return repairView{
		FolderID:        string(r.FolderID),
		DryRun:          r.DryRun,
		MissingDeleted:  orEmpty(r.MissingDeleted),
		Rehashed:        orEmpty(r.Rehashed),
		TransfersReset:  orEmpty(r.TransfersReset),
		TransfersPruned: orEmpty(r.TransfersPruned),
		OrphanTemps:     orEmpty(r.OrphanTemps),
		SequenceFixed:   r.SequenceFixed,
		Unrecoverable:   findings,
	}
}

// orEmpty replaces a nil slice with an empty one so JSON output stays stable.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// newRepairCmd builds the "repair" command.
func newRepairCmd(deps *Deps) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "repair [ID]",
		Short: "Restore consistency between the database and the files on disk",
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

			views := make([]repairView, 0, len(targets))
			changed, unrecoverable := false, 0
			for _, f := range targets {
				report, err := runRepair(cmd.Context(), app, f.ID, dryRun)
				if err != nil {
					return err
				}
				changed = changed || report.Changed()
				unrecoverable += len(report.Unrecoverable)
				views = append(views, viewOfRepair(report))
			}

			if err := printRepair(deps, views); err != nil {
				return err
			}
			switch {
			case unrecoverable > 0:
				return Errorf(ExitIntegrity, "repair: %d path(s) could not be recovered", unrecoverable)
			case dryRun && changed:
				return ErrDryRunChanges
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be repaired without changing anything")
	return cmd
}

// runRepair repairs one folder inside a running daemon, or directly when none
// is running.
func runRepair(ctx context.Context, app *compose.App, id folder.ID, dryRun bool) (repair.Report, error) {
	report, err := app.Control.Repair(ctx, id, dryRun)
	if !errors.Is(err, ports.ErrDaemonUnavailable) {
		return report, err
	}
	return app.Repair.Run(ctx, id, dryRun)
}

// printRepair writes one summary line per folder.
func printRepair(deps *Deps, views []repairView) error {
	if deps.Global.JSON {
		return deps.printer().PrintJSON(views)
	}
	if len(views) == 0 {
		_, err := fmt.Fprintln(deps.Stdout, "no folders to repair")
		return err
	}

	for _, v := range views {
		suffix := ""
		if v.DryRun {
			suffix = " (dry run, nothing written)"
		}
		if _, err := fmt.Fprintf(deps.Stdout,
			"%s: %d missing marked deleted, %d rehashed, %d transfers reset, "+
				"%d failed transfers pruned, %d orphan temp files, sequence fixed: %t%s\n",
			v.FolderID, len(v.MissingDeleted), len(v.Rehashed), len(v.TransfersReset),
			len(v.TransfersPruned), len(v.OrphanTemps), v.SequenceFixed, suffix); err != nil {
			return err
		}
		for _, f := range v.Unrecoverable {
			if _, err := fmt.Fprintf(deps.Stdout, "  unrecoverable: %s (%s)\n", f.Path, f.Reason); err != nil {
				return err
			}
		}
	}
	return nil
}
