package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/syncer"
	"github.com/wesbragagt/syncthis/internal/domain/device"
	"github.com/wesbragagt/syncthis/internal/domain/folder"
)

// planView is the JSON shape of a `sync-plan` run.
type planView struct {
	FolderID string     `json:"folder_id"`
	DryRun   bool       `json:"dry_run"`
	Changes  int        `json:"changes"`
	Peers    []peerPlan `json:"peers"`
}

// peerPlan is what one peer's manifest would mean for the folder.
type peerPlan struct {
	PeerID  string     `json:"peer_id"`
	Name    string     `json:"name"`
	Address string     `json:"address"`
	Note    *string    `json:"note"`
	Changes int        `json:"changes"`
	Items   []planItem `json:"items"`
}

// planItem is one decision of a sync plan.
type planItem struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// planViewOf converts the plans into their JSON shape.
func planViewOf(id folder.ID, plans []syncer.PeerPlan) planView {
	view := planView{FolderID: string(id), DryRun: true, Peers: make([]peerPlan, 0, len(plans))}
	for _, plan := range plans {
		peer := peerPlan{
			PeerID: string(plan.PeerID), Name: plan.Name, Address: plan.Address,
			Note: nonEmpty(plan.Note), Changes: plan.Changes(),
			Items: make([]planItem, 0, len(plan.Items)),
		}
		for _, item := range plan.Items {
			peer.Items = append(peer.Items, planItem{
				Action: item.Action.String(), Path: item.Path, Reason: item.Reason,
			})
		}
		view.Changes += peer.Changes
		view.Peers = append(view.Peers, peer)
	}
	return view
}

// newSyncPlanCmd builds the "sync-plan" command.
func newSyncPlanCmd(deps *Deps) *cobra.Command {
	var peer string

	cmd := &cobra.Command{
		Use:   "sync-plan FOLDER",
		Short: "Show what a sync would change, without changing anything",
		Long: "sync-plan contacts the peers, asks for their manifest and prints what this\n" +
			"device would do with it: what it would pull, create or delete, and why.\n" +
			"Nothing is written. It exits 10 when the folder would change and 0 when it\n" +
			"is already in sync. It does not need a running daemon.",
		Args: requireArgs(1, "expected exactly one folder ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var expect device.ID
			if peer != "" {
				parsed, err := device.ParseID(peer)
				if err != nil {
					return Errorf(ExitUsage, "sync-plan: %v", err)
				}
				expect = parsed
			}

			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			id := folder.ID(args[0])
			plans, err := app.Planner.Plan(cmd.Context(), id, expect)
			if err != nil {
				return err
			}

			view := planViewOf(id, plans)
			if deps.Global.JSON {
				if err := deps.printer().PrintJSON(view); err != nil {
					return err
				}
			} else if err := printPlans(deps, view); err != nil {
				return err
			}
			if view.Changes > 0 {
				return ErrDryRunChanges
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&peer, "peer", "", "only ask this device id (default: every known peer)")
	return cmd
}

// printPlans writes the human-readable form of a dry run.
func printPlans(deps *Deps, view planView) error {
	out := deps.Stdout
	if len(view.Peers) == 0 {
		_, err := fmt.Fprintln(out, "no peers configured")
		return err
	}

	for _, peer := range view.Peers {
		name := peer.Name
		if name == "" {
			name = peer.PeerID
		}
		_, _ = fmt.Fprintf(out, "peer %s (%s) at %s\n", name, peer.PeerID, peer.Address)
		if peer.Note != nil {
			_, _ = fmt.Fprintf(out, "  %s\n", *peer.Note)
			continue
		}
		if len(peer.Items) == 0 {
			_, _ = fmt.Fprintln(out, "  nothing to do")
			continue
		}

		rows := make([][]string, 0, len(peer.Items))
		for _, item := range peer.Items {
			rows = append(rows, []string{item.Action, item.Path, item.Reason})
		}
		if err := (Printer{W: out}).Table([]string{"ACTION", "PATH", "REASON"}, rows); err != nil {
			return err
		}
	}

	if view.Changes == 0 {
		_, err := fmt.Fprintln(out, "\nfolder is in sync; nothing would change")
		return err
	}
	_, err := fmt.Fprintf(out, "\n%d changes would be applied; nothing was changed\n", view.Changes)
	return err
}
