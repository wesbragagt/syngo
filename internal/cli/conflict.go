package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/conflicts"
	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/app/scan"
	"github.com/wesbragagt/syncthis/internal/compose"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
	"github.com/wesbragagt/syncthis/internal/domain/manifest"
)

// conflictView is the JSON shape of one conflict. local and conflict describe
// the two files as they are on disk and are only filled in by the commands
// that look at them.
type conflictView struct {
	ID           string  `json:"id"`
	FolderID     string  `json:"folder_id"`
	Path         string  `json:"path"`
	ConflictPath string  `json:"conflict_path"`
	Reason       string  `json:"reason"`
	LocalHash    string  `json:"local_hash"`
	RemoteHash   string  `json:"remote_hash"`
	PeerID       string  `json:"peer_id"`
	DetectedAt   *string `json:"detected_at"`
	Resolved     bool    `json:"resolved"`
	// File describes what is at path and Copy what is at conflict_path,
	// each saying which of the two versions it holds.
	File *conflictFile `json:"file,omitempty"`
	Copy *conflictFile `json:"copy,omitempty"`
}

// conflictFile is one of the two files of a conflict as it is on disk now.
type conflictFile struct {
	Path    string  `json:"path"`
	Size    int64   `json:"size"`
	ModTime *string `json:"mtime"`
	Hash    string  `json:"hash"`
	// Version is "local", "remote", or "unknown" when the file holds
	// neither of the two versions the conflict recorded.
	Version string `json:"version"`
}

// resolveView is what `conflict resolve` prints.
type resolveView struct {
	ID          string `json:"id"`
	FolderID    string `json:"folder_id"`
	Path        string `json:"path"`
	Kept        string `json:"kept"`
	KeptPath    string `json:"kept_path"`
	MovedFrom   string `json:"moved_from,omitempty"`
	RemovedPath string `json:"removed_path,omitempty"`
	Resolved    bool   `json:"resolved"`
}

// exportView is what `conflict export` prints.
type exportView struct {
	ID      string   `json:"id"`
	Dir     string   `json:"dir"`
	Files   []string `json:"files"`
	Sidecar string   `json:"sidecar"`
}

// conflictViewOf converts a service view into its JSON shape.
func conflictViewOf(v conflicts.View) conflictView {
	return conflictView{
		ID: v.ID, FolderID: string(v.FolderID), Path: v.Path, ConflictPath: v.ConflictPath,
		Reason: v.Reason, LocalHash: v.LocalHash.String(), RemoteHash: v.RemoteHash.String(),
		PeerID: string(v.PeerID), DetectedAt: timestamp(v.DetectedAt), Resolved: v.Resolved,
		File: conflictFileOf(v, v.File), Copy: conflictFileOf(v, v.Copy),
	}
}

// conflictFileOf converts one file of a conflict into its JSON shape.
func conflictFileOf(v conflicts.View, f *conflicts.File) *conflictFile {
	if f == nil {
		return nil
	}
	return &conflictFile{
		Path: f.Path, Size: f.Size, ModTime: timestamp(f.ModTime),
		Hash: f.Hash.String(), Version: versionOf(v, f),
	}
}

// versionOf says which of the two versions a file holds.
func versionOf(v conflicts.View, f *conflicts.File) string {
	switch f.Hash {
	case v.LocalHash:
		return string(conflicts.KeepLocal)
	case v.RemoteHash:
		return string(conflicts.KeepRemote)
	default:
		return "unknown"
	}
}

// newConflictCmd builds the "conflict" command group.
func newConflictCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflict",
		Short: "Inspect and resolve diverged files",
		Long: "A conflict is a path both sides changed. Syncthis keeps both versions:\n" +
			"one stays at the path, the other is written beside it under a\n" +
			"sync-conflict name. These commands show them and say which one wins.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return deps.help(cmd)
		},
	}
	cmd.AddCommand(newConflictListCmd(deps), newConflictShowCmd(deps),
		newConflictResolveCmd(deps), newConflictExportCmd(deps))
	return cmd
}

// newConflictListCmd builds the "conflict list" command.
func newConflictListCmd(deps *Deps) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "list [FOLDER]",
		Short: "List unresolved conflicts",
		Args:  atMostArgs(1, "expected at most one folder ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			var id folder.ID
			if len(args) == 1 {
				id = folder.ID(args[0])
			}
			list, err := app.Conflicts.List(cmd.Context(), id, all)
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				views := make([]conflictView, 0, len(list))
				for _, v := range list {
					views = append(views, conflictViewOf(v))
				}
				return deps.printer().PrintJSON(views)
			}
			if len(list) == 0 {
				_, err := fmt.Fprintln(deps.Stdout, "no conflicts")
				return err
			}

			rows := make([][]string, 0, len(list))
			for _, v := range list {
				rows = append(rows, []string{
					v.ID, string(v.FolderID), v.Path, v.Reason, conflictCopy(v),
					v.PeerID.Short(), v.DetectedAt.UTC().Format(time.RFC3339),
					boolWord(v.Resolved),
				})
			}
			return deps.printer().Table(
				[]string{"ID", "FOLDER", "PATH", "REASON", "COPY", "PEER", "DETECTED", "RESOLVED"}, rows)
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "include conflicts that were already resolved")
	return cmd
}

// newConflictShowCmd builds the "conflict show" command.
func newConflictShowCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
		Short: "Show one conflict and both of its versions",
		Args:  requireArgs(1, "expected exactly one conflict id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			view, err := app.Conflicts.Show(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(conflictViewOf(view))
			}
			return printConflict(deps, view)
		},
	}
}

// newConflictResolveCmd builds the "conflict resolve" command.
func newConflictResolveCmd(deps *Deps) *cobra.Command {
	var keep string

	cmd := &cobra.Command{
		Use:   "resolve ID --keep local|remote|both",
		Short: "Keep one version of a conflict, or both",
		Long: "resolve says which version wins. \"local\" keeps what this device had,\n" +
			"\"remote\" keeps what the peer had, and either removes the other file.\n" +
			"\"both\" changes nothing on disk and only closes the conflict.",
		Args: requireArgs(1, "expected exactly one conflict id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keep == "" {
				return Errorf(ExitUsage, "conflict resolve: --keep is required: local, remote or both")
			}
			choice, err := conflicts.ParseKeep(keep)
			if err != nil {
				return Errorf(ExitUsage, "conflict resolve: %v", err)
			}

			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			view, err := app.Conflicts.Show(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			// Only keeping one version deletes a file, so only that asks.
			if choice != conflicts.KeepBoth {
				if err := deps.confirm(resolveQuestion(view, choice)); err != nil {
					return err
				}
			}

			result, err := app.Conflicts.Resolve(cmd.Context(), args[0], choice)
			if err != nil {
				return err
			}
			// The folder changed, so the stored manifest has to catch up
			// before the next sync cycle plans against it.
			if result.RemovedPath != "" || result.MovedFrom != "" {
				if err := rescanFolder(cmd.Context(), app, result.View.FolderID); err != nil {
					return err
				}
			}
			return printResolution(deps, result)
		},
	}

	cmd.Flags().StringVar(&keep, "keep", "", "which version to keep: local|remote|both")
	return cmd
}

// newConflictExportCmd builds the "conflict export" command.
func newConflictExportCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "export ID DIR",
		Short: "Copy both versions of a conflict into a directory",
		Long: "export writes both versions and a JSON description of the conflict into\n" +
			"DIR, so they can be compared or merged by hand. The folder is not\n" +
			"touched and the conflict stays open.",
		Args: requireArgs(2, "expected a conflict id and a directory"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			result, err := app.Conflicts.Export(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(exportView{
					ID: result.View.ID, Dir: result.Dir, Files: result.Files, Sidecar: result.Sidecar,
				})
			}
			for _, name := range append(result.Files, result.Sidecar) {
				if _, err := fmt.Fprintf(deps.Stdout, "%s/%s\n", result.Dir, escapeCell(name)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// resolveQuestion is what the user confirms before a file is removed.
func resolveQuestion(view conflicts.View, keep conflicts.Keep) string {
	// The file that goes is the one holding the version that loses, which
	// is the path itself when the peer's answer is a deletion.
	loses := ""
	if losing := view.Holder(losingHash(view, keep)); losing != nil {
		loses = losing.Path
	}
	if keep == conflicts.KeepRemote && view.RemoteHash.IsZero() {
		loses = view.Path
	}
	if loses == "" {
		return fmt.Sprintf("keep the %s version of %s", keep, view.Path)
	}
	return fmt.Sprintf("keep the %s version of %s and delete %s", keep, view.Path, loses)
}

// losingHash is the version a resolution does not keep.
func losingHash(view conflicts.View, keep conflicts.Keep) manifestHash {
	if keep == conflicts.KeepLocal {
		return view.RemoteHash
	}
	return view.LocalHash
}

// manifestHash is the content hash type, named here so the printing helpers
// read as what they are about.
type manifestHash = manifest.Hash

// printConflict writes the human-readable form of one conflict.
func printConflict(deps *Deps, v conflicts.View) error {
	out := deps.Stdout
	_, _ = fmt.Fprintf(out, "id:       %s\n", v.ID)
	_, _ = fmt.Fprintf(out, "folder:   %s (%s)\n", v.FolderID, escapeCell(v.FolderPath))
	_, _ = fmt.Fprintf(out, "path:     %s\n", escapeCell(v.Path))
	_, _ = fmt.Fprintf(out, "copy:     %s\n", escapeCell(conflictCopy(v)))
	_, _ = fmt.Fprintf(out, "reason:   %s\n", v.Reason)
	_, _ = fmt.Fprintf(out, "peer:     %s\n", v.PeerID)
	_, _ = fmt.Fprintf(out, "detected: %s\n", v.DetectedAt.UTC().Format(time.RFC3339))
	_, _ = fmt.Fprintf(out, "resolved: %s\n", boolWord(v.Resolved))
	printVersion(deps, "local", v.LocalHash, v)
	printVersion(deps, "remote", v.RemoteHash, v)
	return nil
}

// printVersion writes one of the two versions of a conflict and the file that
// holds it, which is not always the file it started in: a resolution moves a
// version from the copy back onto the path.
func printVersion(deps *Deps, name string, hash manifestHash, v conflicts.View) {
	out := deps.Stdout
	if hash.IsZero() {
		_, _ = fmt.Fprintf(out, "\n%s version: deleted by the peer\n", name)
		return
	}
	_, _ = fmt.Fprintf(out, "\n%s version: %s\n", name, hash)
	file := v.Holder(hash)
	if file == nil {
		_, _ = fmt.Fprintln(out, "  no file holds it any more")
		return
	}
	_, _ = fmt.Fprintf(out, "  file:  %s\n", escapeCell(file.Path))
	_, _ = fmt.Fprintf(out, "  size:  %d bytes\n", file.Size)
	_, _ = fmt.Fprintf(out, "  mtime: %s\n", file.ModTime.UTC().Format(time.RFC3339))
}

// printResolution reports what a resolve did.
func printResolution(deps *Deps, r conflicts.Resolution) error {
	out := deps.Stdout
	if deps.Global.JSON {
		return deps.printer().PrintJSON(resolveView{
			ID: r.View.ID, FolderID: string(r.View.FolderID), Path: r.View.Path,
			Kept: string(r.Kept), KeptPath: r.KeptPath, MovedFrom: r.MovedFrom,
			RemovedPath: r.RemovedPath, Resolved: true,
		})
	}
	switch {
	case r.Kept == conflicts.KeepBoth:
		_, _ = fmt.Fprintf(out, "%s: both versions kept\n", escapeCell(r.View.Path))
	case r.KeptPath == "":
		_, _ = fmt.Fprintf(out, "%s: removed, the peer had deleted it\n", escapeCell(r.View.Path))
	default:
		_, _ = fmt.Fprintf(out, "%s: %s version kept\n", escapeCell(r.KeptPath), r.Kept)
	}
	if r.RemovedPath != "" && r.RemovedPath != r.View.Path {
		_, _ = fmt.Fprintf(out, "%s: removed\n", escapeCell(r.RemovedPath))
	}
	return nil
}

// conflictCopy is the copy holding the other version, or a dash when the
// conflict has none because the peer deleted the path or changed its kind.
func conflictCopy(v conflicts.View) string {
	if v.ConflictPath == "" {
		return "-"
	}
	return v.ConflictPath
}

// boolWord renders a flag as yes or no.
func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// rescanFolder makes the stored manifest catch up with a folder a command
// changed. A running daemon owns the folder, so it does the work; without one
// the command scans by itself.
func rescanFolder(ctx context.Context, app *compose.App, id folder.ID) error {
	err := app.Control.Rescan(ctx, id)
	if !errors.Is(err, ports.ErrDaemonUnavailable) {
		return err
	}
	f, err := app.Folders.Get(ctx, id)
	if err != nil {
		return err
	}
	_, err = app.Scan.Scan(ctx, f, scan.Options{})
	return err
}
