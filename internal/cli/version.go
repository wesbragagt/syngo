package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build information, overridable at link time with -ldflags -X.
var (
	// Version is the released version of the binary.
	Version = "0.1.0-dev"
	// Commit is the git revision the binary was built from.
	Commit = "unknown"
)

// versionInfo is the JSON shape of the version command.
type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
}

// newVersionCmd builds the "version" command.
func newVersionCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the syncthis version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := versionInfo{Version: Version, Commit: Commit, Go: runtime.Version()}
			if deps.Global.JSON {
				return deps.printer().PrintJSON(info)
			}
			_, err := fmt.Fprintf(deps.Stdout, "syncthis %s (%s, %s)\n", info.Version, info.Commit, info.Go)
			return err
		},
	}
}
