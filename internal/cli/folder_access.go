package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syngo/internal/domain/device"
	"github.com/wesbragagt/syngo/internal/domain/folder"
)

// grantView is the JSON shape of one authorization change.
type grantView struct {
	Folder string `json:"folder"`
	Device string `json:"device"`
}

// accessView is the JSON shape of "folder devices".
type accessView struct {
	Folder  string       `json:"folder"`
	Devices []deviceName `json:"devices"`
}

// deviceName is one authorized device, with the name the peer table holds for
// it. The name is empty for a device that is authorized but no longer paired.
type deviceName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// parseGrantArgs reads the folder id and the device id both grant and revoke
// take. A malformed id is the user's mistake, not a missing row.
func parseGrantArgs(command string, args []string) (folder.ID, device.ID, error) {
	id, err := folder.ParseID(args[0])
	if err != nil {
		return "", "", Errorf(ExitUsage, "%s: %v", command, err)
	}
	dev, err := device.ParseID(args[1])
	if err != nil {
		return "", "", Errorf(ExitUsage, "%s: %v", command, err)
	}
	return id, dev, nil
}

// newFolderGrantCmd builds the "folder grant" command.
func newFolderGrantCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "grant ID DEVICE-ID",
		Short: "Allow a device to synchronize a folder",
		Long: "grant authorizes one paired device to synchronize one folder. A folder is\n" +
			"served to no device until it is granted one: a peer that is not authorized\n" +
			"is not told the folder exists. A running daemon is told at once, so the\n" +
			"device starts synchronizing on the connection it already has.",
		Args: requireArgs(2, "expected a folder ID and a device ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, dev, err := parseGrantArgs("folder grant", args)
			if err != nil {
				return err
			}

			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			if err := app.Folders.Grant(cmd.Context(), id, dev); err != nil {
				return err
			}
			reloadDaemon(cmd.Context(), deps, app)

			if deps.Global.JSON {
				return deps.printer().PrintJSON(grantView{Folder: string(id), Device: string(dev)})
			}
			_, err = fmt.Fprintf(deps.Stdout, "device %s may synchronize folder %s\n", dev, id)
			return err
		},
	}
}

// newFolderRevokeCmd builds the "folder revoke" command.
func newFolderRevokeCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke ID DEVICE-ID",
		Short: "Stop a device from synchronizing a folder",
		Long: "revoke withdraws the authorization of one device for one folder. It takes\n" +
			"effect on the connection the device already has, in the middle of a\n" +
			"transfer if there is one: the next request it makes for the folder is\n" +
			"answered as if the folder did not exist. The files already on the other\n" +
			"device are left where they are.",
		Args: requireArgs(2, "expected a folder ID and a device ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, dev, err := parseGrantArgs("folder revoke", args)
			if err != nil {
				return err
			}

			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			if err := app.Folders.Revoke(cmd.Context(), id, dev); err != nil {
				return err
			}
			reloadDaemon(cmd.Context(), deps, app)

			if deps.Global.JSON {
				return deps.printer().PrintJSON(grantView{Folder: string(id), Device: string(dev)})
			}
			_, err = fmt.Fprintf(deps.Stdout, "device %s may no longer synchronize folder %s\n", dev, id)
			return err
		},
	}
}

// newFolderDevicesCmd builds the "folder devices" command.
func newFolderDevicesCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "devices ID",
		Short: "Show which devices may synchronize a folder",
		Args:  requireArgs(1, "expected exactly one folder ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			id := folder.ID(args[0])
			access, err := app.Folders.Access(cmd.Context(), id)
			if err != nil {
				return err
			}
			known, err := app.Peers.List(cmd.Context())
			if err != nil {
				return err
			}
			names := make(map[device.ID]string, len(known))
			for _, d := range known {
				names[d.ID] = d.Name
			}

			authorized := access.Devices()
			if deps.Global.JSON {
				view := accessView{Folder: string(id), Devices: make([]deviceName, 0, len(authorized))}
				for _, dev := range authorized {
					view.Devices = append(view.Devices, deviceName{ID: string(dev), Name: names[dev]})
				}
				return deps.printer().PrintJSON(view)
			}
			if len(authorized) == 0 {
				_, err := fmt.Fprintf(deps.Stdout,
					"folder %s is authorized for no device, so no peer can see it\n", id)
				return err
			}
			rows := make([][]string, 0, len(authorized))
			for _, dev := range authorized {
				rows = append(rows, []string{dev.Display(), names[dev]})
			}
			return deps.printer().Table([]string{"DEVICE ID", "NAME"}, rows)
		},
	}
}
