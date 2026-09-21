package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/transfers"
	"github.com/wesbragagt/syncthis/internal/domain/transfer"
)

// transferView is the JSON shape of one transfer. It repeats the field names
// of the transfers section of `status --json`, so both can be parsed alike.
type transferView struct {
	ID          string  `json:"id"`
	FolderID    string  `json:"folder_id"`
	Path        string  `json:"path"`
	PeerID      string  `json:"peer_id"`
	Direction   string  `json:"direction"`
	State       string  `json:"state"`
	Size        int64   `json:"size"`
	ChunkSize   int     `json:"chunk_size"`
	ChunksTotal int     `json:"chunks_total"`
	ChunksDone  int     `json:"chunks_done"`
	BytesDone   int64   `json:"bytes_done"`
	CreatedAt   *string `json:"created_at"`
	UpdatedAt   *string `json:"updated_at"`
	Error       *string `json:"error"`
	// ChunksMissing and MissingChunks are only filled in by
	// `transfer show`.
	ChunksMissing *int  `json:"chunks_missing,omitempty"`
	MissingChunks []int `json:"missing_chunks,omitempty"`
}

// transferViewOf converts a transfer view into its JSON shape.
func transferViewOf(v transfers.View) transferView {
	out := transferView{
		ID: v.ID, FolderID: string(v.FolderID), Path: v.Path, PeerID: v.PeerID,
		Direction: "in", State: string(v.State), Size: v.Size, ChunkSize: v.ChunkSize,
		ChunksTotal: v.ChunksTotal, ChunksDone: v.ChunksDone, BytesDone: v.BytesDone,
		CreatedAt: timestamp(v.CreatedAt), UpdatedAt: timestamp(v.UpdatedAt),
		Error: nonEmpty(v.Error),
	}
	if v.MissingChunks != nil {
		missing := len(v.MissingChunks)
		out.ChunksMissing = &missing
		out.MissingChunks = v.MissingChunks
	}
	return out
}

// timestamp formats t as an RFC3339 UTC string, or nil when it is zero.
func timestamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// newTransferCmd builds the "transfer" command group.
func newTransferCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transfer",
		Short: "Inspect file transfers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newTransferListCmd(deps), newTransferShowCmd(deps))
	return cmd
}

// newTransferListCmd builds the "transfer list" command.
func newTransferListCmd(deps *Deps) *cobra.Command {
	var state string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List unfinished and failed transfers",
		Long: "list shows the transfer journal. A transfer disappears from it once its\n" +
			"file is installed, so what is left is running, paused or failed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			filter := transfers.Filter{}
			if state != "" {
				parsed, err := parseTransferState(state)
				if err != nil {
					return Errorf(ExitUsage, "transfer list: %v", err)
				}
				filter.State = parsed
			}

			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			list, err := app.Transfers.List(cmd.Context(), filter)
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				views := make([]transferView, 0, len(list))
				for _, v := range list {
					views = append(views, transferViewOf(v))
				}
				return deps.printer().PrintJSON(views)
			}
			if len(list) == 0 {
				_, err := fmt.Fprintln(deps.Stdout, "no transfers")
				return err
			}

			rows := make([][]string, 0, len(list))
			for _, v := range list {
				rows = append(rows, []string{
					v.ID, string(v.FolderID), v.Path, string(v.State),
					fmt.Sprintf("%d/%d", v.ChunksDone, v.ChunksTotal),
					strconv.FormatInt(v.BytesDone, 10),
					strconv.FormatInt(v.Size, 10),
					v.PeerID,
				})
			}
			return deps.printer().Table(
				[]string{"ID", "FOLDER", "PATH", "STATE", "CHUNKS", "DONE", "SIZE", "PEER"}, rows)
		},
	}

	cmd.Flags().StringVar(&state, "state", "", "only transfers in this state: "+strings.Join(transferStates(), "|"))
	return cmd
}

// newTransferShowCmd builds the "transfer show" command.
func newTransferShowCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "show TRANSFER_ID",
		Short: "Show one transfer and how far it got",
		Args:  requireArgs(1, "expected exactly one transfer id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			view, err := app.Transfers.Show(cmd.Context(), transfer.ID(args[0]))
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(transferViewOf(view))
			}
			return printTransfer(deps, view)
		},
	}
}

// missingShown is how many missing chunk indexes the text output names before
// it stops listing them.
const missingShown = 10

// printTransfer writes the human-readable form of one transfer.
func printTransfer(deps *Deps, v transfers.View) error {
	out := deps.Stdout
	_, _ = fmt.Fprintf(out, "id:        %s\n", v.ID)
	_, _ = fmt.Fprintf(out, "folder:    %s\n", v.FolderID)
	_, _ = fmt.Fprintf(out, "path:      %s\n", escapeCell(v.Path))
	_, _ = fmt.Fprintf(out, "peer:      %s\n", v.PeerID)
	_, _ = fmt.Fprintf(out, "direction: in\n")
	_, _ = fmt.Fprintf(out, "state:     %s\n", v.State)
	_, _ = fmt.Fprintf(out, "size:      %d bytes (%d done)\n", v.Size, v.BytesDone)
	_, _ = fmt.Fprintf(out, "chunks:    %d of %d done, %d bytes each\n",
		v.ChunksDone, v.ChunksTotal, v.ChunkSize)
	_, _ = fmt.Fprintf(out, "missing:   %d%s\n", len(v.MissingChunks), indexList(v.MissingChunks))
	if !v.UpdatedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "updated:   %s\n", v.UpdatedAt.UTC().Format(time.RFC3339))
	}
	if v.Error != "" {
		_, _ = fmt.Fprintf(out, "error:     %s\n", v.Error)
	}
	return nil
}

// indexList renders the first missing chunk indexes, or nothing when there are
// none.
func indexList(indexes []int) string {
	if len(indexes) == 0 {
		return ""
	}
	shown := indexes
	suffix := ""
	if len(shown) > missingShown {
		shown = shown[:missingShown]
		suffix = ", ..."
	}
	parts := make([]string, 0, len(shown))
	for _, idx := range shown {
		parts = append(parts, strconv.Itoa(idx))
	}
	return " (" + strings.Join(parts, ", ") + suffix + ")"
}

// transferStates lists the states a transfer may be filtered by.
func transferStates() []string {
	return []string{
		string(transfer.StatePending), string(transfer.StateActive), string(transfer.StatePaused),
		string(transfer.StateVerifying), string(transfer.StateDone), string(transfer.StateFailed),
	}
}

// parseTransferState validates a --state value.
func parseTransferState(s string) (transfer.State, error) {
	for _, known := range transferStates() {
		if s == known {
			return transfer.State(s), nil
		}
	}
	return "", fmt.Errorf("unknown transfer state %q, want one of %s", s, strings.Join(transferStates(), ", "))
}
