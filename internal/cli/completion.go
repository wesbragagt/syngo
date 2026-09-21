package cli

import (
	"github.com/spf13/cobra"
)

// shells are the shells completion scripts can be generated for.
var shells = []string{"bash", "zsh", "fish", "powershell"}

// newCompletionCmd builds the "completion" command.
func newCompletionCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:       "completion " + "bash|zsh|fish|powershell",
		Short:     "Generate a shell completion script",
		Args:      requireArgs(1, "expected exactly one shell: bash|zsh|fish|powershell"),
		ValidArgs: shells,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(deps.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(deps.Stdout)
			case "fish":
				return root.GenFishCompletion(deps.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(deps.Stdout)
			default:
				return Errorf(ExitUsage, "unsupported shell %q, want one of %v", args[0], shells)
			}
		},
	}
}
