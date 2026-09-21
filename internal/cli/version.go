package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// devVersion is what Version holds in a binary that was built without
// -ldflags. The Makefile sets both variables from `git describe`.
const devVersion = "0.1.0-dev"

// Build information, overridable at link time with -ldflags -X.
var (
	// Version is the released version of the binary.
	Version = devVersion
	// Commit is the git revision the binary was built from.
	Commit = "unknown"
)

// init fills in what the go tool recorded when the binary was built without
// -ldflags, which is what `go install` and `go run` produce.
func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if Version == devVersion && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}
	if Commit != "unknown" {
		return
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			Commit = setting.Value
		}
	}
}

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
