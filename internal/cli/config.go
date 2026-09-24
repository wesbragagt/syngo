package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/wesbragagt/syngo/internal/app/ports"
	"github.com/wesbragagt/syngo/internal/compose"
	"github.com/wesbragagt/syngo/internal/domain/bandwidth"
)

// configView is the JSON shape `config show` and `config set` print. The rates
// are bytes per second, 0 for unlimited; the intervals are Go durations.
type configView struct {
	Listen         string `json:"listen"`
	Interface      string `json:"interface"`
	ChunkSize      int    `json:"chunk_size"`
	MaxFileSize    int64  `json:"max_file_size"`
	RescanInterval string `json:"rescan_interval"`
	SyncInterval   string `json:"sync_interval"`
	LogLevel       string `json:"log_level"`
	UploadLimit    int64  `json:"upload_limit"`
	DownloadLimit  int64  `json:"download_limit"`
	TextMerge      bool   `json:"text_merge"`
}

// configViewOf describes settings the way the command prints them.
func configViewOf(s ports.Settings) configView {
	return configView{
		Listen:         s.Listen,
		Interface:      s.Interface,
		ChunkSize:      s.ChunkSize,
		MaxFileSize:    s.MaxFileSize,
		RescanInterval: s.RescanInterval.String(),
		SyncInterval:   s.SyncInterval.String(),
		LogLevel:       s.LogLevel,
		UploadLimit:    int64(s.UploadLimit),
		DownloadLimit:  int64(s.DownloadLimit),
		TextMerge:      s.TextMerge,
	}
}

// newConfigCmd builds the "config" command group.
func newConfigCmd(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and change the device settings",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return deps.help(cmd)
		},
	}
	cmd.AddCommand(newConfigShowCmd(deps), newConfigSetCmd(deps),
		newConfigExportCmd(deps), newConfigImportCmd(deps))
	return cmd
}

// newConfigShowCmd builds the "config show" command.
func newConfigShowCmd(deps *Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the settings in config.json",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := deps.app(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			settings, err := app.Settings.Load(cmd.Context())
			if err != nil {
				return err
			}
			return printConfig(deps, settings)
		},
	}
}

// newConfigSetCmd builds the "config set" command. Every flag is optional and
// at least one is required; the settings the flags do not name are left alone.
func newConfigSetCmd(deps *Deps) *cobra.Command {
	var (
		uploadLimit    string
		downloadLimit  string
		rescanInterval time.Duration
		syncInterval   time.Duration
		listen         string
		iface          string
		logLevel       string
		textMerge      bool
	)

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Change one or more settings in config.json",
		Long: "set writes the named settings to config.json and asks a running daemon to\n" +
			"reload. The bandwidth limits take effect at once; the other settings take\n" +
			"effect when the daemon next starts.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !anyChanged(cmd, "upload-limit", "download-limit", "rescan-interval",
				"sync-interval", "listen", "interface", "log-level", "text-merge") {
				return Errorf(ExitUsage, "%s: expected at least one setting to change", cmd.CommandPath())
			}

			app, err := deps.app(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer func() { _ = app.Close() }()

			settings, err := app.Settings.Load(cmd.Context())
			if err != nil {
				return err
			}
			if err := applyFlags(cmd, &settings, configFlags{
				uploadLimit: uploadLimit, downloadLimit: downloadLimit,
				rescanInterval: rescanInterval, syncInterval: syncInterval,
				listen: listen, iface: iface, logLevel: logLevel,
				textMerge: textMerge,
			}); err != nil {
				return err
			}
			if err := app.Settings.Save(cmd.Context(), settings); err != nil {
				return err
			}
			// The daemon re-reads the settings on reload, which is
			// what makes a new limit hold for the connections it
			// already has.
			reloadDaemon(cmd.Context(), deps, app)
			return printConfig(deps, settings)
		},
	}

	cmd.Flags().StringVar(&uploadLimit, "upload-limit", "",
		"outgoing chunk rate, for example 4MiB, 500KB or 0 for unlimited")
	cmd.Flags().StringVar(&downloadLimit, "download-limit", "",
		"incoming chunk rate, for example 4MiB, 500KB or 0 for unlimited")
	cmd.Flags().DurationVar(&rescanInterval, "rescan-interval", 0, "how often every folder is fully rescanned")
	cmd.Flags().DurationVar(&syncInterval, "sync-interval", 0, "how often a connected peer is asked what changed")
	cmd.Flags().StringVar(&listen, "listen", "", "address peers connect to")
	cmd.Flags().StringVar(&iface, "interface", "", "network interface to bind the first IPv4 address of")
	cmd.Flags().StringVar(&logLevel, "log-level", "", "log level the daemon starts with: debug|info|warn|error")
	cmd.Flags().BoolVar(&textMerge, "text-merge", false,
		"merge concurrent edits of a text file instead of keeping both versions")
	return cmd
}

// configFlags are the raw values `config set` was given.
type configFlags struct {
	uploadLimit    string
	downloadLimit  string
	rescanInterval time.Duration
	syncInterval   time.Duration
	listen         string
	iface          string
	logLevel       string
	textMerge      bool
}

// applyFlags validates the flags that were given and writes them into
// settings. Nothing is written when one of them is refused, so a bad value
// never leaves the configuration half changed.
func applyFlags(cmd *cobra.Command, settings *ports.Settings, f configFlags) error {
	if cmd.Flags().Changed("upload-limit") {
		rate, err := parseRateFlag("--upload-limit", f.uploadLimit)
		if err != nil {
			return err
		}
		settings.UploadLimit = rate
	}
	if cmd.Flags().Changed("download-limit") {
		rate, err := parseRateFlag("--download-limit", f.downloadLimit)
		if err != nil {
			return err
		}
		settings.DownloadLimit = rate
	}
	if cmd.Flags().Changed("rescan-interval") {
		if f.rescanInterval <= 0 {
			return Errorf(ExitUsage, "--rescan-interval %s: must be positive", f.rescanInterval)
		}
		settings.RescanInterval = f.rescanInterval
	}
	if cmd.Flags().Changed("sync-interval") {
		if f.syncInterval <= 0 {
			return Errorf(ExitUsage, "--sync-interval %s: must be positive", f.syncInterval)
		}
		settings.SyncInterval = f.syncInterval
	}
	if cmd.Flags().Changed("listen") {
		if err := compose.ValidateListen(f.listen); err != nil {
			return err
		}
		settings.Listen = f.listen
	}
	if cmd.Flags().Changed("interface") {
		settings.Interface = f.iface
	}
	if cmd.Flags().Changed("log-level") {
		if !contains(logLevels, f.logLevel) {
			return Errorf(ExitUsage, "invalid --log-level %q, want one of %v", f.logLevel, logLevels)
		}
		settings.LogLevel = f.logLevel
	}
	if cmd.Flags().Changed("text-merge") {
		settings.TextMerge = f.textMerge
	}
	return nil
}

// parseRateFlag reads a rate flag, reporting a bad one as a usage error.
func parseRateFlag(name, value string) (bandwidth.Rate, error) {
	rate, err := bandwidth.ParseRate(value)
	if err != nil {
		return 0, Errorf(ExitUsage, "%s %q: %w", name, value, err)
	}
	return rate, nil
}

// anyChanged reports whether any of the named flags was given.
func anyChanged(cmd *cobra.Command, names ...string) bool {
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

// printConfig writes the settings as JSON or as key = value lines.
func printConfig(deps *Deps, s ports.Settings) error {
	if deps.Global.JSON {
		return deps.printer().PrintJSON(configViewOf(s))
	}
	lines := [][2]string{
		{"listen", s.Listen},
		{"interface", s.Interface},
		{"chunk_size", fmt.Sprint(s.ChunkSize)},
		{"max_file_size", fmt.Sprint(s.MaxFileSize)},
		{"rescan_interval", s.RescanInterval.String()},
		{"sync_interval", s.SyncInterval.String()},
		{"log_level", s.LogLevel},
		{"upload_limit", s.UploadLimit.String()},
		{"download_limit", s.DownloadLimit.String()},
		{"text_merge", strconv.FormatBool(s.TextMerge)},
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(deps.Stdout, "%s = %s\n", line[0], line[1]); err != nil {
			return err
		}
	}
	return nil
}
