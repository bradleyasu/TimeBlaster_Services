// Package logging configures Timeblaster's structured logging.
//
// The daemon runs under systemd, so logs go to stderr and journald captures
// them. Timestamps are omitted from the text format because journald adds its
// own, and a duplicated timestamp on every line makes `journalctl -u
// timeblaster.service` needlessly hard to read.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configure the logger.
type Options struct {
	// Level is debug, info, warn or error.
	Level string
	// Format is "text" (logfmt, journald-friendly) or "json".
	Format string
	// Output defaults to stderr.
	Output io.Writer
	// IncludeTime adds a timestamp. Leave it off under systemd; turn it on when
	// running the daemon by hand.
	IncludeTime bool
	// Source adds file:line, which is useful when chasing a specific log line
	// back to the code but too noisy for normal operation.
	Source bool
}

// New builds a logger.
func New(opts Options) *slog.Logger {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{
		Level:     ParseLevel(opts.Level),
		AddSource: opts.Source,
	}
	if !opts.IncludeTime {
		handlerOpts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}

	var h slog.Handler
	if strings.EqualFold(opts.Format, "json") {
		h = slog.NewJSONHandler(out, handlerOpts)
	} else {
		h = slog.NewTextHandler(out, handlerOpts)
	}
	return slog.New(h)
}

// ParseLevel converts a level name to a slog.Level, defaulting to info.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Discard returns a logger that writes nowhere, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}
