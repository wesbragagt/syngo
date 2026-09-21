package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/status"
)

// newStatusCmd builds the "status" command.
func newStatusCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show device, folder, peer and transfer status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			// The daemon is not reachable yet; live information arrives with
			// the control socket.
			report, err := app.Status.Report(cmd.Context(), nil)
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(report)
			}
			return printStatus(deps, report)
		},
	}
}

// printStatus writes the human-readable status output.
func printStatus(deps *Deps, r status.Report) error {
	out := deps.Stdout
	_, _ = fmt.Fprintf(out, "device:  %s (%s)\n", r.Device.Name, r.Device.ID)
	_, _ = fmt.Fprintf(out, "datadir: %s\n", r.Device.DataDir)
	if r.Daemon.Running {
		_, _ = fmt.Fprintf(out, "daemon:  running (pid %d, listening on %s)\n", r.Daemon.PID, r.Daemon.Listen)
	} else {
		_, _ = fmt.Fprintln(out, "daemon:  not running")
	}

	_, _ = fmt.Fprintln(out)
	if len(r.Folders) == 0 {
		_, _ = fmt.Fprintln(out, "no folders configured")
	} else {
		rows := make([][]string, 0, len(r.Folders))
		for _, f := range r.Folders {
			rows = append(rows, []string{
				f.ID, f.Path, f.Mode, f.State,
				strconv.FormatInt(f.Files, 10),
				strconv.FormatInt(f.Bytes, 10),
			})
		}
		if err := (Printer{W: out}).Table([]string{"FOLDER", "PATH", "MODE", "STATE", "FILES", "BYTES"}, rows); err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintln(out)
	if len(r.Peers) == 0 {
		_, _ = fmt.Fprintln(out, "no peers configured")
	} else {
		rows := make([][]string, 0, len(r.Peers))
		for _, p := range r.Peers {
			rows = append(rows, []string{p.ID, p.Name, p.Address, strconv.FormatBool(p.Connected)})
		}
		if err := (Printer{W: out}).Table([]string{"PEER", "NAME", "ADDRESS", "CONNECTED"}, rows); err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintf(out, "\ntransfers: %d active, %d pending, %d paused, %d failed\n",
		r.Transfers.Active, r.Transfers.Pending, r.Transfers.Paused, r.Transfers.Failed)
	_, _ = fmt.Fprintf(out, "conflicts: %d unresolved\n", r.Conflicts.Unresolved)
	return nil
}
