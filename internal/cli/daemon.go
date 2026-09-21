package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/compose"
)

// newDaemonCmd builds the "daemon" command group.
func newDaemonCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run and control the background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return deps.help(cmd)
		},
	}
	cmd.AddCommand(newDaemonRunCmd(deps))
	return cmd
}

// daemonStarted is the JSON shape `daemon run --json` prints once the daemon
// answers. It is the only document the command writes to stdout; everything
// else it has to say goes to the log on stderr.
type daemonStarted struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	DataDir string `json:"data_dir"`
}

// printDaemonStarted announces a daemon that is serving requests.
func printDaemonStarted(deps *Deps, dataDir string) error {
	if deps.Global.JSON {
		return deps.printer().PrintJSON(daemonStarted{Running: true, PID: os.Getpid(), DataDir: dataDir})
	}
	_, err := fmt.Fprintf(deps.Stdout, "syncthis daemon running (pid %d, data dir %s)\n", os.Getpid(), dataDir)
	return err
}

// newDaemonRunCmd builds the "daemon run" command.
func newDaemonRunCmd(deps *Deps) *cobra.Command {
	var (
		listen         string
		iface          string
		allowPublic    bool
		rescanInterval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Watch the configured folders and serve the control socket",
		Long: "run watches every configured folder, rescans them periodically and answers\n" +
			"control requests on <data-dir>/control.sock until it is interrupted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// The banner is printed once the daemon is serving, so a
			// second instance fails without claiming to have started.
			ready := make(chan struct{})
			announced := make(chan struct{})
			go func() {
				defer close(announced)
				select {
				case <-ready:
					_ = printDaemonStarted(deps, app.DataDir)
				case <-ctx.Done():
				}
			}()

			err = app.RunDaemon(ctx, compose.DaemonOptions{
				Listen:         listen,
				Interface:      iface,
				AllowPublic:    allowPublic,
				RescanInterval: rescanInterval,
				Ready:          ready,
			})
			stop()
			<-announced
			return err
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address to accept peer connections on (default from config.json)")
	cmd.Flags().StringVar(&iface, "interface", "", "bind to the first IPv4 address of this network interface")
	cmd.Flags().BoolVar(&allowPublic, "allow-public", false,
		"allow binding an address reachable outside this machine and its private network")
	cmd.Flags().DurationVar(&rescanInterval, "rescan-interval", 0,
		"how often every folder is fully rescanned (default from config.json)")
	return cmd
}
