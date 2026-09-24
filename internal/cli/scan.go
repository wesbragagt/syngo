package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syngo/internal/app/scan"
	"github.com/wesbragagt/syngo/internal/domain/folder"
	"github.com/wesbragagt/syngo/internal/domain/manifest"
)

// changeView is the JSON shape of one classified change.
type changeView struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	OldPath string `json:"old_path"`
	Hash    string `json:"hash"`
}

// summaryView is the JSON shape of the scan counters.
type summaryView struct {
	Scanned    int   `json:"scanned"`
	Hashed     int   `json:"hashed"`
	Bytes      int64 `json:"bytes"`
	Ignored    int   `json:"ignored"`
	Skipped    int   `json:"skipped"`
	Changes    int   `json:"changes"`
	DurationMS int64 `json:"duration_ms"`
	DryRun     bool  `json:"dry_run"`
}

// scanView is the JSON shape of one scanned folder.
type scanView struct {
	FolderID string       `json:"folder_id"`
	Changes  []changeView `json:"changes"`
	Summary  summaryView  `json:"summary"`
	// Warnings describes the entries the scan could not read.
	Warnings []string `json:"warnings"`
}

// viewOfResult converts a scan result into its JSON shape.
func viewOfResult(r scan.Result) scanView {
	changes := make([]changeView, 0, len(r.Changes))
	for _, c := range r.Changes {
		changes = append(changes, changeView{
			Type: c.Type.String(), Path: c.Path, OldPath: c.OldPath, Hash: hashOf(c),
		})
	}
	warnings := r.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	return scanView{
		FolderID: string(r.FolderID),
		Changes:  changes,
		Summary: summaryView{
			Scanned: r.Scanned, Hashed: r.Hashed, Bytes: r.Bytes, Ignored: r.Ignored,
			Skipped: r.Skipped, Changes: len(r.Changes),
			DurationMS: r.Duration.Milliseconds(), DryRun: r.DryRun,
		},
		Warnings: warnings,
	}
}

// hashOf returns the content hash a change leads to, or the one it removed.
func hashOf(c manifest.Change) string {
	if c.New != nil {
		return c.New.Hash.String()
	}
	if c.Old != nil {
		return c.Old.Hash.String()
	}
	return ""
}

// newScanCmd builds the "scan" command.
func newScanCmd(deps *Deps) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "scan ID",
		Short: "Scan a folder and classify what changed",
		Args:  requireArgs(1, "expected exactly one folder ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			f, err := app.Folders.Get(cmd.Context(), folder.ID(args[0]))
			if err != nil {
				return err
			}
			result, err := app.Scan.Scan(cmd.Context(), f, scan.Options{DryRun: dryRun})
			if err != nil {
				return err
			}
			if err := printScan(deps, result); err != nil {
				return err
			}
			if dryRun && len(result.Changes) > 0 {
				return ErrDryRunChanges
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "classify without writing anything")
	return cmd
}

// printScan writes the change list and the summary of one scan.
func printScan(deps *Deps, r scan.Result) error {
	view := viewOfResult(r)
	if deps.Global.JSON {
		return deps.printer().PrintJSON(view)
	}

	if len(view.Changes) > 0 {
		rows := make([][]string, 0, len(view.Changes))
		for _, c := range view.Changes {
			rows = append(rows, []string{c.Type, c.Path, c.OldPath, short(c.Hash)})
		}
		if err := deps.printer().Table([]string{"TYPE", "PATH", "OLD PATH", "HASH"}, rows); err != nil {
			return err
		}
	}

	for _, warning := range view.Warnings {
		if _, err := fmt.Fprintln(deps.Stderr, "syngo: skipped", warning); err != nil {
			return err
		}
	}

	suffix := ""
	if r.DryRun {
		suffix = " (dry run, nothing written)"
	}
	_, err := fmt.Fprintf(deps.Stdout, "%s: %d change(s); scanned %d, hashed %d, ignored %d, skipped %d%s\n",
		view.FolderID, view.Summary.Changes, view.Summary.Scanned,
		view.Summary.Hashed, view.Summary.Ignored, view.Summary.Skipped, suffix)
	return err
}

// short abbreviates a hash for the table output.
func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
