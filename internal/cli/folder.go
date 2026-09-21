package cli

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// folderView is the JSON shape of one configured folder.
type folderView struct {
	ID             string   `json:"id"`
	Path           string   `json:"path"`
	Label          string   `json:"label"`
	Mode           string   `json:"mode"`
	DeletePolicy   string   `json:"delete_policy"`
	Symlinks       string   `json:"symlinks"`
	IncludeHidden  bool     `json:"include_hidden"`
	KeepEmptyDirs  bool     `json:"keep_empty_dirs"`
	IgnorePatterns []string `json:"ignore_patterns"`
	Paused         bool     `json:"paused"`
}

// viewOf converts a domain folder into its JSON shape.
func viewOf(f folder.Folder) folderView {
	patterns := f.IgnorePatterns
	if patterns == nil {
		patterns = []string{}
	}
	return folderView{
		ID: string(f.ID), Path: f.Path, Label: f.Label, Mode: string(f.Mode),
		DeletePolicy: string(f.DeletePolicy), Symlinks: string(f.Symlinks),
		IncludeHidden: f.IncludeHidden, KeepEmptyDirs: f.KeepEmptyDirs,
		IgnorePatterns: patterns, Paused: f.Paused,
	}
}

// newFolderCmd builds the "folder" command group.
func newFolderCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "folder",
		Short: "Manage synchronized folders",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newFolderAddCmd(deps), newFolderListCmd(deps), newFolderRemoveCmd(deps))
	return cmd
}

// folderAddOpts holds the flags of "folder add".
type folderAddOpts struct {
	id            string
	label         string
	mode          string
	deletePolicy  string
	symlinks      string
	includeHidden bool
	noEmptyDirs   bool
	ignore        []string
}

// newFolderAddCmd builds the "folder add" command.
func newFolderAddCmd(deps *Deps) *cobra.Command {
	var opts folderAddOpts

	cmd := &cobra.Command{
		Use:   "add PATH --id ID",
		Short: "Start synchronizing a directory",
		Args:  requireArgs(1, "expected exactly one PATH"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.id == "" {
				return Errorf(ExitUsage, "folder add: --id is required")
			}
			id, err := folder.ParseID(opts.id)
			if err != nil {
				return Errorf(ExitUsage, "folder add: %v", err)
			}
			path, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("resolve path %q: %w", args[0], err)
			}

			f := folder.Folder{
				ID:             id,
				Path:           filepath.Clean(path),
				Label:          opts.label,
				Mode:           folder.Mode(opts.mode),
				DeletePolicy:   folder.DeletePolicy(opts.deletePolicy),
				Symlinks:       folder.SymlinkPolicy(opts.symlinks),
				IncludeHidden:  opts.includeHidden,
				KeepEmptyDirs:  !opts.noEmptyDirs,
				IgnorePatterns: opts.ignore,
			}
			if err := f.Validate(); err != nil {
				return Errorf(ExitUsage, "folder add: %v", err)
			}

			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			added, err := app.Folders.Add(cmd.Context(), f)
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(viewOf(added))
			}
			_, err = fmt.Fprintf(deps.Stdout, "added folder %s at %s\n", added.ID, added.Path)
			return err
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.id, "id", "", "folder id, [a-z0-9][a-z0-9._-]{0,63} (required)")
	f.StringVar(&opts.label, "label", "", "human-readable label")
	f.StringVar(&opts.mode, "mode", string(folder.ModeSendReceive), "sendreceive|sendonly|receiveonly")
	f.StringVar(&opts.deletePolicy, "delete-policy", string(folder.DeletePropagate), "propagate|never")
	f.StringVar(&opts.symlinks, "symlinks", string(folder.SymlinkSkip), "skip|store")
	f.BoolVar(&opts.includeHidden, "include-hidden", false, "synchronize dot files too")
	f.BoolVar(&opts.noEmptyDirs, "no-empty-dirs", false, "do not keep empty directories")
	f.StringArrayVar(&opts.ignore, "ignore", nil, "ignore pattern; repeatable")
	return cmd
}

// newFolderListCmd builds the "folder list" command.
func newFolderListCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured folders",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			list, err := app.Folders.List(cmd.Context())
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				views := make([]folderView, 0, len(list))
				for _, f := range list {
					views = append(views, viewOf(f))
				}
				return deps.printer().PrintJSON(views)
			}
			if len(list) == 0 {
				_, err := fmt.Fprintln(deps.Stdout, "no folders configured")
				return err
			}

			rows := make([][]string, 0, len(list))
			for _, f := range list {
				rows = append(rows, []string{
					string(f.ID), f.Path, f.Label, string(f.Mode), strconv.FormatBool(f.Paused),
				})
			}
			return deps.printer().Table([]string{"ID", "PATH", "LABEL", "MODE", "PAUSED"}, rows)
		},
	}
}

// newFolderRemoveCmd builds the "folder remove" command.
func newFolderRemoveCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "remove ID",
		Short: "Stop synchronizing a folder, leaving its files on disk",
		Args:  requireArgs(1, "expected exactly one folder ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			id := folder.ID(args[0])
			f, err := app.Folders.Get(cmd.Context(), id)
			if err != nil {
				return err
			}
			if err := deps.confirm(fmt.Sprintf("remove folder %s (%s) from syncthis", f.ID, f.Path)); err != nil {
				return err
			}
			if err := app.Folders.Remove(cmd.Context(), id); err != nil {
				return err
			}

			if deps.Global.JSON {
				return deps.printer().PrintJSON(map[string]any{"removed": string(id)})
			}
			_, err = fmt.Fprintf(deps.Stdout, "removed folder %s\n", id)
			return err
		},
	}
}
