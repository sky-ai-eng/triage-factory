// Package logging configures the process-wide structured logger (slog) and
// hands out per-component child loggers.
//
// Configuration is read from the environment at package-init time. Go
// initializes an imported package (including its init function) before the
// package that imports it, and every package that logs imports this one — so
// the slog default is fully configured before any other package's
// package-level logger vars are constructed. That ordering is what lets
// callers write, at package scope:
//
//	var routerLog = logging.Component("router")
//
// and still observe the final configuration.
//
// Environment knobs:
//
//	TF_LOG_LEVEL   debug | info | warn | error   (default: info)
//	TF_LOG_FORMAT  text | json                    (default: json in multi
//	                                               mode, text otherwise)
//
// Every record carries a "component" attribute — the structured replacement
// for the old "[prefix]" log tags:
//
//	routerLog.Info("task bumped", "task", taskID, "events", n)
//	  → level=INFO msg="task bumped" component=router task=... events=...
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

const (
	// envLevel sets the minimum level emitted. See ParseLevel for accepted
	// values; anything else falls back to info.
	envLevel = "TF_LOG_LEVEL"
	// envFormat selects the handler: "text" (human-readable, the default for
	// local/CLI use) or "json" (machine-parseable, the default in multi mode).
	envFormat = "TF_LOG_FORMAT"
	// envMode mirrors internal/runmode's TF_MODE. It is read directly rather
	// than through that package so logging stays dependency-free and can
	// configure itself at init time; only the default log *format* keys off
	// it (json when multi).
	envMode = "TF_MODE"
)

// levelVar is the shared, mutable minimum level. Every component logger routes
// through one handler that reads this var, so SetLevel takes effect live on
// loggers that were already handed out by Component.
var levelVar = new(slog.LevelVar)

func init() {
	levelVar.Set(ParseLevel(os.Getenv(envLevel)))
	slog.SetDefault(slog.New(newHandler(os.Stderr)))
}

// newHandler builds the configured handler over w at the shared level.
func newHandler(w io.Writer) slog.Handler {
	opts := &slog.HandlerOptions{Level: levelVar}
	if jsonFormat() {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// jsonFormat reports whether the JSON handler should be used: an explicit
// TF_LOG_FORMAT wins, otherwise JSON is the default only in multi mode.
func jsonFormat() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envFormat))) {
	case "json":
		return true
	case "text":
		return false
	default:
		return strings.EqualFold(strings.TrimSpace(os.Getenv(envMode)), "multi")
	}
}

// Component returns a logger that tags every record with component=name. Call
// it once per component — typically a package-level var — rather than per log
// call.
func Component(name string) *slog.Logger {
	return slog.Default().With("component", name)
}

// SetLevel changes the global minimum level at runtime (e.g. from a CLI flag
// parsed after init). It affects loggers already handed out by Component
// because they share the level var behind a single handler.
func SetLevel(l slog.Level) { levelVar.Set(l) }

// Level reports the current minimum level.
func Level() slog.Level { return levelVar.Level() }

// ParseLevel maps a level name (case-insensitive) to a slog.Level, defaulting
// to Info for empty or unrecognized input.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
