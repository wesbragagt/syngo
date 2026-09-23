package cli

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/tui"
)

// cliRunner is the adapter behind tui.Runner: it runs the syncthis command
// tree in this process and hands back what the command wrote to stdout and the
// code it would have exited with.
//
// This is the whole of the TUI's access to the daemon, and it is deliberate.
// Every screen is a view of a command a user could have typed, so there is no
// operation the TUI can perform that the command line cannot, no second copy
// of an application rule inside the presentation layer, and no wire format to
// keep in step: `status` still reaches the running daemon over the control
// socket and still falls back to the database without one, because it is the
// same code path the command line takes. The cost is one process-local command
// invocation per refresh, which is what the poll interval is sized for.
type cliRunner struct {
	// stderr is where a command's diagnostics go. It is the TUI's own
	// stderr, which is not the terminal the interface draws on.
	stderr io.Writer
}

// Run implements tui.Runner.
func (r cliRunner) Run(ctx context.Context, args []string) ([]byte, int, error) {
	var stdout bytes.Buffer
	code := ExecuteContext(ctx, args, nopReader{}, &stdout, r.stderr)
	return stdout.Bytes(), code, nil
}

// nopReader is the stdin a TUI-issued command gets. Every call the TUI makes
// carries --json, which implies --no-input, so nothing ever reads it; an empty
// reader makes a command that tried anyway fail at once instead of blocking
// the interface.
type nopReader struct{}

// Read implements io.Reader.
func (nopReader) Read([]byte) (int, error) { return 0, io.EOF }

// newTUICmd builds the "tui" command.
//
// The interface is a subcommand of syncthis rather than a second binary
// because syncthis ships as one binary: `make dist` builds ./cmd/syncthis
// alone, the packaging, the container image and the Nix flake install that one
// file, and shell completion is generated from this command tree. A separate
// syncthis-tui would have to be added to all five. cmd/syncthis-bench is the
// precedent for a second binary and it is a development tool that is not
// shipped, which the TUI is not.
func newTUICmd(deps *Deps) *cobra.Command {
	var (
		poll    time.Duration
		logFile string
	)

	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive terminal interface",
		Long: "tui shows devices, folders, transfers, paths, conflicts and logs, and\n" +
			"walks a new device through initialization and pairing. Every action it\n" +
			"offers is one syncthis command; docs/TUI.md lists which.\n\n" +
			"It holds no state of its own: it reads the running daemon the way the\n" +
			"commands do, so closing the terminal, losing an SSH session or killing\n" +
			"the process leaves the daemon and every transfer in flight untouched.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deps.Global.JSON {
				return Errorf(ExitUsage, "syncthis tui: there is no JSON form of an interactive interface")
			}
			client := tui.Client{
				Runner:  cliRunner{stderr: deps.Stderr},
				DataDir: deps.Global.DataDir,
			}
			opts := tui.Options{Client: client, Poll: poll, LogFile: logFile}
			return tui.Run(cmd.Context(), opts, deps.Stdin, deps.Stdout)
		},
	}
	cmd.Flags().DurationVar(&poll, "poll", tui.DefaultPoll,
		"how often the live views ask the daemon what changed")
	cmd.Flags().StringVar(&logFile, "log-file", "",
		"a file `daemon run` is writing its log to, for the searchable log view")
	return cmd
}
