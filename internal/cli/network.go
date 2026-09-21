package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/network"
)

// detectView is the JSON shape of `network tailscale detect`.
type detectView struct {
	Present        bool     `json:"present"`
	Running        bool     `json:"running"`
	BackendState   string   `json:"backend_state"`
	Version        string   `json:"version"`
	Interface      string   `json:"interface"`
	HostName       string   `json:"hostname"`
	DNSName        string   `json:"dns_name"`
	IPs            []string `json:"ips"`
	MagicDNSSuffix string   `json:"magic_dns_suffix"`
	// Error says why an installed client could not answer. It is null when
	// it answered, and when there is no client to ask.
	Error *string `json:"error"`
}

// detectViewOf converts a detection into its JSON shape.
func detectViewOf(d network.Detection) detectView {
	ips := d.IPs
	if ips == nil {
		ips = []string{}
	}
	return detectView{
		Present: d.Present, Running: d.Running, BackendState: d.BackendState,
		Version: d.Version, Interface: d.Interface, HostName: d.HostName,
		DNSName: d.DNSName, IPs: ips, MagicDNSSuffix: d.MagicDNSSuffix,
		Error: nonEmpty(d.Err),
	}
}

// tailnetPeerView is the JSON shape of one node in `network peers`.
type tailnetPeerView struct {
	ID       string   `json:"id"`
	HostName string   `json:"hostname"`
	DNSName  string   `json:"dns_name"`
	IPs      []string `json:"ips"`
	Online   bool     `json:"online"`
	OS       string   `json:"os"`
}

// newNetworkCmd builds the "network" command group.
func newNetworkCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Inspect the network this device synchronizes over",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return deps.help(cmd)
		},
	}
	cmd.AddCommand(newNetworkTailscaleCmd(deps), newNetworkPeersCmd(deps))
	return cmd
}

// newNetworkTailscaleCmd builds the "network tailscale" command group.
func newNetworkTailscaleCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tailscale",
		Short: "Ask the local Tailscale client about this device",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return deps.help(cmd)
		},
	}
	cmd.AddCommand(newNetworkTailscaleDetectCmd(deps))
	return cmd
}

// newNetworkTailscaleDetectCmd builds the "network tailscale detect" command.
func newNetworkTailscaleDetectCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "Report whether Tailscale is installed and running here",
		Long: "detect runs `tailscale status --json` and reports what it said. A device\n" +
			"without Tailscale is not a failure: the command prints present: false and\n" +
			"exits 0, because detection asked a question and \"no\" is an answer. A client\n" +
			"that is installed and cannot answer prints present: true with the reason in\n" +
			"error, and also exits 0.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			detection, err := app.Network.Detect(cmd.Context())
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(detectViewOf(detection))
			}
			printDetection(deps, detection)
			return nil
		},
	}
}

// printDetection writes the human-readable form of a detection.
func printDetection(deps *Deps, d network.Detection) {
	out := deps.Stdout
	if !d.Present {
		_, _ = fmt.Fprintln(out, "tailscale: not installed")
		return
	}
	_, _ = fmt.Fprintf(out, "tailscale:        installed\n")
	if d.Err != "" {
		// The binary is there and could not answer, so nothing else is
		// known and only the reason is worth printing.
		_, _ = fmt.Fprintf(out, "error:            %s\n", d.Err)
		return
	}
	_, _ = fmt.Fprintf(out, "version:          %s\n", d.Version)
	_, _ = fmt.Fprintf(out, "backend state:    %s\n", d.BackendState)
	_, _ = fmt.Fprintf(out, "running:          %t\n", d.Running)
	_, _ = fmt.Fprintf(out, "interface:        %s\n", orNone(d.Interface, "userspace networking"))
	_, _ = fmt.Fprintf(out, "hostname:         %s\n", d.HostName)
	_, _ = fmt.Fprintf(out, "dns name:         %s\n", d.DNSName)
	_, _ = fmt.Fprintf(out, "ips:              %s\n", orNone(strings.Join(d.IPs, ", "), "none"))
	_, _ = fmt.Fprintf(out, "magic dns suffix: %s\n", orNone(d.MagicDNSSuffix, "none"))
	_, _ = fmt.Fprintf(out, "error:            none\n")
}

// orNone returns s, or the replacement when s is empty.
func orNone(s, replacement string) string {
	if s == "" {
		return replacement
	}
	return s
}

// newNetworkPeersCmd builds the "network peers" command.
func newNetworkPeersCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "peers",
		Short: "List the devices on this tailnet",
		Long: "peers lists what the local Tailscale client knows about the other devices\n" +
			"of the tailnet. These are not syncthis peers: pair one with `peer add`.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			nodes, err := app.Network.Peers(cmd.Context())
			if err != nil {
				return err
			}
			if deps.Global.JSON {
				views := make([]tailnetPeerView, 0, len(nodes))
				for _, n := range nodes {
					views = append(views, tailnetPeerView{
						ID: n.ID, HostName: n.HostName, DNSName: n.DNSName,
						IPs: nonNil(n.IPs), Online: n.Online, OS: n.OS,
					})
				}
				return deps.printer().PrintJSON(views)
			}
			if len(nodes) == 0 {
				_, err := fmt.Fprintln(deps.Stdout, "no other devices on this tailnet")
				return err
			}

			rows := make([][]string, 0, len(nodes))
			for _, n := range nodes {
				rows = append(rows, []string{
					n.HostName, n.DNSName, strings.Join(n.IPs, ","),
					strconv.FormatBool(n.Online), n.OS,
				})
			}
			return deps.printer().Table([]string{"HOSTNAME", "DNS NAME", "IPS", "ONLINE", "OS"}, rows)
		},
	}
}

// nonNil returns s, or an empty slice when s is nil, so the JSON holds [] and
// not null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
