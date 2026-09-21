package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/compose"
)

// initResult is the JSON shape of the init command.
type initResult struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	DataDir  string `json:"data_dir"`
	Listen   string `json:"listen"`
	Created  bool   `json:"created"`
}

// newInitCmd builds the "init" command.
func newInitCmd(deps *Deps) *cobra.Command {
	var settings compose.InitSettings

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the data directory, device identity and configuration",
		Long: "init prepares the data directory: it generates the device identity, writes the\n" +
			"default configuration and creates the database. Running it again prints the\n" +
			"existing device identity and changes nothing else.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if settings.DeviceName == "" {
				name, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("determine device name: %w", err)
				}
				settings.DeviceName = name
			}

			app, created, err := compose.Init(cmd.Context(), compose.Options{DataDir: deps.Global.DataDir}, settings)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			result := initResult{
				DeviceID: string(app.Identity.DeviceID()),
				Name:     app.Identity.Name(),
				DataDir:  app.DataDir,
				Listen:   app.Config.Listen,
				Created:  created,
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(result)
			}

			state := "already initialized"
			if created {
				state = "initialized"
			}
			_, err = fmt.Fprintf(deps.Stdout, "%s %s\ndevice id: %s\ndevice name: %s\nlisten: %s\n",
				state, result.DataDir, result.DeviceID, result.Name, result.Listen)
			return err
		},
	}

	cmd.Flags().StringVar(&settings.DeviceName, "name", "", "device name (default: hostname)")
	cmd.Flags().StringVar(&settings.Listen, "listen", "", "address the daemon listens on (default 127.0.0.1:22000)")
	return cmd
}
