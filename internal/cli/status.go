package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/app/status"
)

// newStatusCmd builds the "status" command.
func newStatusCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show device, folder, peer and transfer status",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			// A running daemon knows what the database cannot say:
			// which folders are scanning, and its own pid and uptime.
			report, err := app.Control.Status(cmd.Context())
			if errors.Is(err, ports.ErrDaemonUnavailable) {
				report, err = app.Status.Report(cmd.Context(), nil)
			}
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

// printListener writes the one line that says how the peer port is bound, and
// a second one when it has lost its address at some point: that is what an
// operator needs to see after a Tailscale restart or a suspend.
func printListener(out io.Writer, l status.Listener) {
	bound := "bound"
	if !l.Bound {
		bound = "not bound, rebinding"
	}
	iface := l.Interface
	if iface == "" {
		iface = "no interface configured"
	}
	_, _ = fmt.Fprintf(out, "listener: %s on %s (%s)\n", bound, l.Address, iface)
	if l.LostAt == nil && l.Failures == 0 {
		return
	}
	lost := "never"
	if l.LostAt != nil {
		lost = *l.LostAt
	}
	reason := "none"
	if l.Error != nil {
		reason = *l.Error
	}
	_, _ = fmt.Fprintf(out, "          last lost %s, rebinds %d, failed attempts %d, last error: %s\n",
		lost, l.Rebinds, l.Failures, reason)
}

// printStatus writes the human-readable status output.
func printStatus(deps *Deps, r status.Report) error {
	out := deps.Stdout
	_, _ = fmt.Fprintf(out, "device:  %s (%s)\n", r.Device.Name, r.Device.ID)
	_, _ = fmt.Fprintf(out, "datadir: %s\n", r.Device.DataDir)
	if r.Daemon.Running {
		_, _ = fmt.Fprintf(out, "daemon:  running (pid %d, listening on %s)\n", r.Daemon.PID, r.Daemon.Listen)
		printListener(out, r.Daemon.Listener)
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
			rows = append(rows, []string{p.ID, p.Name, p.Address, p.State, p.Path.Kind})
		}
		if err := (Printer{W: out}).Table([]string{"PEER", "NAME", "ADDRESS", "STATE", "PATH"}, rows); err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintf(out, "\ntransfers: %d active, %d pending, %d paused, %d failed\n",
		r.Transfers.Active, r.Transfers.Pending, r.Transfers.Paused, r.Transfers.Failed)
	_, _ = fmt.Fprintf(out, "conflicts: %d unresolved\n", r.Conflicts.Unresolved)
	return nil
}
