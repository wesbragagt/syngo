package cli

import (
	"context"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/compose"
)

// Deps are the streams and options a command tree needs. Later milestones add
// the composition root here.
type Deps struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Global GlobalOpts
}

// app wires the adapters and services for the current data directory.
// requireIdentity makes the call fail with ExitNotInitialized when the data
// directory has not been initialized yet.
func (d *Deps) app(ctx context.Context, requireIdentity bool) (*compose.App, error) {
	return compose.Build(ctx, compose.Options{
		DataDir:         d.Global.DataDir,
		Logger:          d.logger(),
		RequireIdentity: requireIdentity,
	})
}

// help prints the usage of a command group. With --json the help text would
// be the only non-machine-readable thing on stdout, so the missing subcommand
// is reported as a usage error instead.
func (d *Deps) help(cmd *cobra.Command) error {
	if d.Global.JSON {
		return Errorf(ExitUsage, "%s: expected a subcommand", cmd.CommandPath())
	}
	return cmd.Help()
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
			return deps.help(cmd)
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
	root.AddCommand(
		newVersionCmd(deps),
		newCompletionCmd(deps),
		newInitCmd(deps),
		newFolderCmd(deps),
		newStatusCmd(deps),
		newScanCmd(deps),
		newRescanCmd(deps),
		newRepairCmd(deps),
		newDaemonCmd(deps),
		newPeerCmd(deps),
		newTransferCmd(deps),
		newSyncPlanCmd(deps),
		newPauseCmd(deps),
		newResumeCmd(deps),
	)
	return root
}

// Execute runs the CLI with the given arguments and streams, and returns the
// process exit code.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return ExecuteContext(context.Background(), args, stdin, stdout, stderr)
}

// ExecuteContext runs the CLI with a context commands inherit. Cancelling it
// stops long-running commands such as `daemon run`.
func ExecuteContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	deps := &Deps{Stdin: stdin, Stdout: stdout, Stderr: stderr, Global: defaultGlobalOpts()}
	root := NewRootCmd(deps)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	code := codeOf(err)
	// A bare exit code carries no message: the command printed its own
	// output already, as dry runs do.
	var bare *ExitCodeError
	if errors.As(err, &bare) && bare.Err == nil {
		return code
	}
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
