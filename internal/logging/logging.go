// Package logging configures the process-wide slog logger per ARCHITECTURE
// engineering standards: structured fields, level-correct semantics, no
// interpolated strings.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// SetupDefault installs a JSON slog handler on os.Stderr as the package
// default logger. Returns the handler's level so callers can adjust dynamically
// if needed (v0 does not).
func SetupDefault(level string) *slog.LevelVar {
	lv := new(slog.LevelVar)
	lv.Set(parseLevel(level))

	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     lv,
		AddSource: false,
	})
	slog.SetDefault(slog.New(h))
	return lv
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		fallthrough
	default:
		return slog.LevelInfo
	}
}
