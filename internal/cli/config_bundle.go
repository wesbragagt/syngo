package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syngo/internal/app/configbundle"
	"github.com/wesbragagt/syngo/internal/compose"
)

// bundleFileMode is what `config export --output` writes with. The document
// holds no secret, but the data directory is 0700 and an export of it should
// not be looser.
const bundleFileMode = 0o600

// stdioPath is the file name both bundle commands read as "use the standard
// stream".
const stdioPath = "-"

// newConfigExportCmd builds the "config export" command.
func newConfigExportCmd(deps *Deps) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "export [--output FILE]",
		Short: "Write the settings, folders and peers as one JSON document",
		Long: "export prints the configuration of this device as one JSON document:\n" +
			"the settings, the folders with their ignore patterns and sync windows,\n" +
			"and the known peers. The identity key, the certificate, the device name\n" +
			"and the database contents are never part of it.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			bundle, err := app.Bundle.Export(cmd.Context())
			if err != nil {
				return err
			}
			if output == "" || output == stdioPath {
				// The bundle is JSON already, so --json changes
				// nothing about what is printed.
				return configbundle.Encode(deps.Stdout, bundle)
			}

			var buf strings.Builder
			if err := configbundle.Encode(&buf, bundle); err != nil {
				return err
			}
			if err := os.WriteFile(output, []byte(buf.String()), bundleFileMode); err != nil {
				return fmt.Errorf("write %s: %w", output, err)
			}
			// WriteFile only applies the mode to a file it creates,
			// so an export over a looser file would keep that mode.
			if err := os.Chmod(output, bundleFileMode); err != nil {
				return fmt.Errorf("chmod %s: %w", output, err)
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(map[string]any{
					"written": output, "folders": len(bundle.Folders), "peers": len(bundle.Peers),
				})
			}
			_, err = fmt.Fprintf(deps.Stdout, "exported %s and %s to %s\n",
				count(len(bundle.Folders), "folder"), count(len(bundle.Peers), "peer"), output)
			return err
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "file to write, 0600; - or unset writes to stdout")
	return cmd
}

// newConfigImportCmd builds the "config import" command.
func newConfigImportCmd(deps *Deps) *cobra.Command {
	var replace, dryRun bool

	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Apply a configuration bundle to this device",
		Long: "import reads a document written by `config export`, validates all of it\n" +
			"before writing any of it, and adds the folders, the peers and the\n" +
			"settings it names. A folder id or a peer that is already configured is\n" +
			"a conflict unless --replace is given; a path already synchronized under\n" +
			"another folder id is always a conflict. The identity of this device is\n" +
			"never touched. FILE may be - to read the document from stdin.",
		Args: requireArgs(1, "expected exactly one FILE"),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundle, err := readBundle(deps, args[0])
			if err != nil {
				return err
			}
			// A dry run writes nothing, so --replace has nothing
			// destructive to confirm.
			if replace && !dryRun {
				if err := deps.confirm(
					"replace the folders and peers this bundle names"); err != nil {
					return err
				}
			}

			app, err := deps.app(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			// Conflict detection and the writes have to be one
			// import against one data directory, or two at once
			// both report what only one of them wrote.
			unlock, err := compose.LockImport(app.DataDir)
			if err != nil {
				return err
			}
			defer unlock()

			result, err := app.Bundle.Import(cmd.Context(), bundle,
				configbundle.ImportOptions{Replace: replace, DryRun: dryRun})
			if err != nil {
				return importFailed(deps, result, err)
			}
			if !dryRun {
				reloadDaemon(cmd.Context(), deps, app)
			}
			if err := printImport(deps, result); err != nil {
				return err
			}
			if dryRun && result.Changed() {
				return ErrDryRunChanges
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&replace, "replace", false,
		"overwrite the folders and peers whose id the bundle repeats")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report what would change and write nothing; exits 10 when something would")
	return cmd
}

// readBundle reads and decodes a bundle from a file, or from stdin for "-". An
// unreadable file and a document that does not decode are both mistakes in
// what the command was given, so both exit 2.
func readBundle(deps *Deps, path string) (configbundle.Bundle, error) {
	r := deps.Stdin
	if path != stdioPath {
		f, err := os.Open(path)
		if err != nil {
			return configbundle.Bundle{}, Errorf(ExitUsage, "config import: %v", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	bundle, err := configbundle.Decode(r)
	if err != nil {
		return configbundle.Bundle{}, err
	}
	return bundle, nil
}

// importFailed reports an import that did not go through. Conflicts are listed
// one per line before the error, and a store that failed midway is reported
// together with what was written.
func importFailed(deps *Deps, written configbundle.Result, cause error) error {
	var conflicts *configbundle.ConflictError
	if errors.As(cause, &conflicts) {
		if !deps.Global.JSON {
			for _, c := range conflicts.Conflicts {
				_, _ = fmt.Fprintln(deps.Stderr, c.String())
			}
		}
		return cause
	}
	if !written.Changed() {
		return cause
	}
	if deps.Global.JSON {
		return fmt.Errorf("%w (written: %s)", cause, summarize(written))
	}
	// The result goes out before the error, so a partial import says what
	// it did land before it says why it stopped.
	if err := printImport(deps, written); err != nil {
		return err
	}
	return cause
}

// printImport writes the result of an import.
func printImport(deps *Deps, r configbundle.Result) error {
	if deps.Global.JSON {
		return deps.printer().PrintJSON(r)
	}
	lines := importLines(r)
	if len(lines) == 0 {
		_, err := fmt.Fprintln(deps.Stdout, "nothing to import")
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(deps.Stdout, line); err != nil {
			return err
		}
	}
	return nil
}

// importLines renders one line per item an import touched.
func importLines(r configbundle.Result) []string {
	var out []string
	for _, id := range r.FoldersAdded {
		out = append(out, fmt.Sprintf("folder %s: added", id))
	}
	for _, id := range r.FoldersReplaced {
		out = append(out, fmt.Sprintf("folder %s: replaced", id))
	}
	for _, id := range r.PeersAdded {
		out = append(out, fmt.Sprintf("peer %s: added", id))
	}
	for _, id := range r.PeersReplaced {
		out = append(out, fmt.Sprintf("peer %s: replaced", id))
	}
	if r.SettingsChanged {
		out = append(out, "settings: changed")
	}
	return out
}

// summarize renders what an import wrote in one line, for the JSON error
// message of a partial import.
func summarize(r configbundle.Result) string {
	lines := importLines(r)
	if len(lines) == 0 {
		return "nothing"
	}
	return strings.Join(lines, "; ")
}

// count renders "1 folder" or "2 folders".
func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
