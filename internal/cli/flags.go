package cli

import (
	"github.com/spf13/cobra"

	"github.com/wesbragagt/syncthis/internal/app/ports"
)

// Log levels and formats accepted by the global flags. The levels are the ones
// an imported bundle is checked against too.
var (
	logLevels  = ports.LogLevels
	logFormats = []string{"text", "json"}
)

// GlobalOpts holds the flags every command shares.
type GlobalOpts struct {
	DataDir   string
	JSON      bool
	NoInput   bool
	Yes       bool
	LogLevel  string
	LogFormat string
}

// defaultGlobalOpts returns the global flag defaults.
func defaultGlobalOpts() GlobalOpts {
	return GlobalOpts{LogLevel: "info", LogFormat: "text"}
}

// register binds the global flags to o on cmd's persistent flag set.
func (o *GlobalOpts) register(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&o.DataDir, "data-dir", o.DataDir, "data directory (default $SYNCTHIS_DATA_DIR or $XDG_DATA_HOME/syncthis)")
	f.BoolVar(&o.JSON, "json", o.JSON, "emit machine-readable JSON")
	f.BoolVar(&o.NoInput, "no-input", o.NoInput, "never prompt; fail instead")
	f.BoolVar(&o.Yes, "yes", o.Yes, "assume yes for confirmations")
	f.StringVar(&o.LogLevel, "log-level", o.LogLevel, "log level: debug|info|warn|error")
	f.StringVar(&o.LogFormat, "log-format", o.LogFormat, "log format: text|json")
}

// validate checks the global flag values.
func (o *GlobalOpts) validate() error {
	if !contains(logLevels, o.LogLevel) {
		return Errorf(ExitUsage, "invalid --log-level %q, want one of %v", o.LogLevel, logLevels)
	}
	if !contains(logFormats, o.LogFormat) {
		return Errorf(ExitUsage, "invalid --log-format %q, want one of %v", o.LogFormat, logFormats)
	}
	return nil
}

// contains reports whether want is in list.
func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
