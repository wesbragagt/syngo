package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syngo/internal/app/peers"
	"github.com/wesbragagt/syngo/internal/domain/device"
)

// peerView is the JSON shape of one known peer.
type peerView struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Address    string  `json:"address"`
	Verified   bool    `json:"verified"`
	AddedAt    string  `json:"added_at"`
	LastSeenAt *string `json:"last_seen_at"`
	// Accepted reports whether the peer already knows this device. It is
	// only filled in by `peer add`.
	Accepted *bool `json:"accepted,omitempty"`
	// ResolvedFrom is the host name the tailnet turned into Address. It is
	// absent when the address was used as typed.
	ResolvedFrom string `json:"resolved_from,omitempty"`
}

// peerViewOf converts a device into its JSON shape.
func peerViewOf(d device.Device) peerView {
	view := peerView{
		ID:       string(d.ID),
		Name:     d.Name,
		Address:  d.Address,
		Verified: d.Verified,
		AddedAt:  d.AddedAt.UTC().Format(time.RFC3339),
	}
	if !d.LastSeenAt.IsZero() {
		seen := d.LastSeenAt.UTC().Format(time.RFC3339)
		view.LastSeenAt = &seen
	}
	return view
}

// testView is the JSON shape of a `peer test` result.
type testView struct {
	Address       string  `json:"address"`
	DeviceID      string  `json:"device_id"`
	Name          string  `json:"name"`
	TLSOK         bool    `json:"tls_ok"`
	Authenticated bool    `json:"authenticated"`
	RejectReason  string  `json:"reject_reason"`
	Reason        string  `json:"reason"`
	LatencyMS     float64 `json:"latency_ms"`
	Error         *string `json:"error"`
	// ResolvedFrom is the host name the tailnet turned into Address.
	ResolvedFrom string `json:"resolved_from,omitempty"`
}

// testViewOf converts a test result into its JSON shape.
func testViewOf(r peers.TestResult) testView {
	return testView{
		Address:       r.Address,
		DeviceID:      string(r.DeviceID),
		Name:          r.Name,
		TLSOK:         r.TLSOK,
		Authenticated: r.Authenticated,
		RejectReason:  r.RejectReason,
		Reason:        r.Reason,
		LatencyMS:     float64(r.Latency.Microseconds()) / 1000,
		Error:         nonEmpty(r.Err),
		ResolvedFrom:  r.ResolvedFrom,
	}
}

// nonEmpty returns a pointer to s, or nil when s is empty.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// newPeerCmd builds the "peer" command group.
func newPeerCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer",
		Short: "Manage the devices this one synchronizes with",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return deps.help(cmd)
		},
	}
	cmd.AddCommand(newPeerAddCmd(deps), newPeerListCmd(deps), newPeerRemoveCmd(deps), newPeerTestCmd(deps))
	return cmd
}

// newPeerAddCmd builds the "peer add" command.
func newPeerAddCmd(deps *Deps) *cobra.Command {
	var (
		deviceID string
		name     string
		verify   bool
	)

	cmd := &cobra.Command{
		Use:   "add HOST[:PORT]",
		Short: "Trust another device and remember how to reach it",
		Long: "add contacts the device listening at HOST[:PORT], shows the identity it\n" +
			"presents and stores it. Pass --device-id to pin the identity instead of\n" +
			"confirming it, and run the same command on the other device to finish pairing.",
		Args: requireArgs(1, "expected exactly one HOST[:PORT]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var expect device.ID
			if deviceID != "" {
				parsed, err := device.ParseID(deviceID)
				if err != nil {
					return Errorf(ExitUsage, "peer add: %v", err)
				}
				expect = parsed
			}

			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			result, err := app.Peers.Add(cmd.Context(), peers.AddOptions{
				Address:  args[0],
				ExpectID: expect,
				Name:     name,
				Verify:   verify,
				Confirm:  deps.confirmDevice,
			})
			if err != nil {
				return err
			}
			reloadDaemon(cmd.Context(), deps, app)

			if deps.Global.JSON {
				view := peerViewOf(result.Device)
				view.Accepted = &result.Accepted
				view.ResolvedFrom = result.ResolvedFrom
				return deps.printer().PrintJSON(view)
			}
			_, _ = fmt.Fprintf(deps.Stdout, "added peer %s (%s) at %s%s\n",
				result.Device.Name, result.Device.ID.Display(), result.Device.Address,
				viaTailnet(result.ResolvedFrom))
			if !result.Accepted {
				_, _ = fmt.Fprintf(deps.Stdout,
					"the peer has not added this device yet; run `syngo peer add` there too\n")
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&deviceID, "device-id", "", "expected device id; the peer is refused if it presents another")
	f.StringVar(&name, "name", "", "name to store for the peer (default: the name it announces)")
	f.BoolVar(&verify, "verify", false, "confirm the identity even when --device-id pins it")
	return cmd
}

// confirmDevice shows an identity and asks whether to trust it.
func (d *Deps) confirmDevice(id device.ID, name string) (bool, error) {
	if !d.Global.JSON {
		_, _ = fmt.Fprintf(d.Stdout, "device id: %s\nname:      %s\n", id.Display(), name)
	}
	if err := d.confirm("trust this device"); err != nil {
		return false, err
	}
	return true, nil
}

// newPeerListCmd builds the "peer list" command.
func newPeerListCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List known peers",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			list, err := app.Peers.List(cmd.Context())
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				views := make([]peerView, 0, len(list))
				for _, p := range list {
					views = append(views, peerViewOf(p))
				}
				return deps.printer().PrintJSON(views)
			}
			if len(list) == 0 {
				_, err := fmt.Fprintln(deps.Stdout, "no peers configured")
				return err
			}

			rows := make([][]string, 0, len(list))
			for _, p := range list {
				seen := "never"
				if !p.LastSeenAt.IsZero() {
					seen = p.LastSeenAt.UTC().Format(time.RFC3339)
				}
				rows = append(rows, []string{
					string(p.ID), p.Name, p.Address, strconv.FormatBool(p.Verified), seen,
				})
			}
			return deps.printer().Table([]string{"PEER", "NAME", "ADDRESS", "VERIFIED", "LAST SEEN"}, rows)
		},
	}
}

// newPeerRemoveCmd builds the "peer remove" command.
func newPeerRemoveCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "remove ID",
		Short: "Forget a peer",
		Args:  requireArgs(1, "expected exactly one device ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			if err := deps.confirm(fmt.Sprintf("remove peer %s", args[0])); err != nil {
				return err
			}
			if err := app.Peers.Remove(cmd.Context(), device.ID(args[0])); err != nil {
				return err
			}
			reloadDaemon(cmd.Context(), deps, app)

			if deps.Global.JSON {
				return deps.printer().PrintJSON(map[string]any{"removed": args[0]})
			}
			_, err = fmt.Fprintf(deps.Stdout, "removed peer %s\n", args[0])
			return err
		},
	}
}

// newPeerTestCmd builds the "peer test" command.
func newPeerTestCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "test ID|HOST[:PORT]",
		Short: "Check whether a peer answers and accepts this device",
		Args:  requireArgs(1, "expected one device ID or HOST[:PORT]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := deps.app(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			result, err := app.Peers.Test(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			// In JSON mode a failure is reported as the error object,
			// so the result is only printed when the peer answered.
			if deps.Global.JSON {
				if !result.Authenticated {
					return Errorf(ExitPeerUnreachable, "peer test %s: %s: %s",
						result.Address, result.Reason, result.Err)
				}
				return deps.printer().PrintJSON(testViewOf(result))
			}

			printTestResult(deps, result)
			if !result.Authenticated {
				return &ExitCodeError{Code: ExitPeerUnreachable}
			}
			return nil
		},
	}
}

// viaTailnet names the host the tailnet resolved, or nothing when the address
// was used as typed.
func viaTailnet(name string) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf(" (%s via tailnet)", name)
}

// printTestResult writes the human-readable form of a connection test.
func printTestResult(deps *Deps, r peers.TestResult) {
	out := deps.Stdout
	_, _ = fmt.Fprintf(out, "address:       %s%s\n", r.Address, viaTailnet(r.ResolvedFrom))
	if r.DeviceID != "" {
		_, _ = fmt.Fprintf(out, "device id:     %s\n", r.DeviceID.Display())
	}
	if r.Name != "" {
		_, _ = fmt.Fprintf(out, "name:          %s\n", r.Name)
	}
	_, _ = fmt.Fprintf(out, "tls:           %t\n", r.TLSOK)
	_, _ = fmt.Fprintf(out, "authenticated: %t\n", r.Authenticated)
	if r.Authenticated {
		_, _ = fmt.Fprintf(out, "latency:       %.2f ms\n", float64(r.Latency.Microseconds())/1000)
		return
	}
	_, _ = fmt.Fprintf(out, "reason:        %s\n", r.Reason)
	if r.Err != "" {
		_, _ = fmt.Fprintf(out, "error:         %s\n", r.Err)
	}
}
