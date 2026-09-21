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
			return cmd.Help()
		},
	}
	cmd.AddCommand(newDaemonRunCmd(deps))
	return cmd
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
					_, _ = fmt.Fprintf(deps.Stdout, "syncthis daemon running (pid %d, data dir %s)\n",
						os.Getpid(), app.DataDir)
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
