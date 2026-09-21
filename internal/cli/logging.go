package cli

import (
	"log/slog"
	"strings"
)

// logger builds the structured logger from the global --log-level and
// --log-format flags. It writes to stderr so stdout stays machine-readable.
func (d *Deps) logger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: logLevel(d.Global.LogLevel)}
	if strings.EqualFold(d.Global.LogFormat, "json") {
		return slog.New(slog.NewJSONHandler(d.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(d.Stderr, opts))
}

// logLevel maps a flag value to a slog level.
func logLevel(name string) slog.Level {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
