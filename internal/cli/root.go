package cli

import (
	"io"

	"github.com/spf13/cobra"
)

// Deps are the streams and options a command tree needs. Later milestones add
// the composition root here.
type Deps struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Global GlobalOpts
}

// printer returns a Printer honouring the --json flag.
func (d *Deps) printer() Printer {
	return Printer{JSON: d.Global.JSON, W: d.Stdout}
}

// NewRootCmd builds the syncthis command tree bound to deps.
func NewRootCmd(deps *Deps) *cobra.Command {
	root := &cobra.Command{
		Use:   "syncthis",
		Short: "Peer-to-peer file synchronization",
		Long:  "syncthis keeps folders in sync between devices you own.",
		// Errors and usage are printed by Execute so that --json output
		// stays machine-readable.
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return deps.Global.validate()
		},
		// Args and RunE together replace cobra's own unknown-command
		// handling so the failure carries ExitUsage.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return Errorf(ExitUsage, "unknown command %q for %q", args[0], cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	root.SetIn(deps.Stdin)
	root.SetOut(deps.Stdout)
	root.SetErr(deps.Stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return Errorf(ExitUsage, "%s: %w", cmd.CommandPath(), err)
	})

	deps.Global.register(root)
	root.AddCommand(newVersionCmd(deps), newCompletionCmd(deps))
	return root
}

// Execute runs the CLI with the given arguments and streams, and returns the
// process exit code.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	deps := &Deps{Stdin: stdin, Stdout: stdout, Stderr: stderr, Global: defaultGlobalOpts()}
	root := NewRootCmd(deps)
	root.SetArgs(args)

	err := root.Execute()
	if err == nil {
		return ExitOK
	}

	code := codeOf(err)
	// The --json flag may not have been parsed yet when the failure is a
	// usage error, so fall back to scanning the raw arguments.
	if deps.Global.JSON || hasJSONFlag(args) {
		printError(stdout, true, code, err)
	} else {
		printError(stderr, false, code, err)
	}
	return code
}

// hasJSONFlag reports whether --json appears in the raw arguments.
func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" || a == "--json=true" {
			return true
		}
		if a == "--" {
			return false
		}
	}
	return false
}

// requireArgs returns a cobra argument validator that reports usage errors
// with the correct exit code.
func requireArgs(n int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return Errorf(ExitUsage, "%s: %s", cmd.CommandPath(), usage)
		}
		return nil
	}
}
