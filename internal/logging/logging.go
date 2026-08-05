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
//
// A binary that calls SetProcess (the cap-broker/orchestrator privilege
// split) additionally gets a "proc" attribute on every record, so a
// broker's and an orchestrator's interleaved stdout stay distinguishable
// after boot even though every "component" tag they emit (router,
// sandbox, ...) can otherwise overlap between the two processes.
//
// Records emitted through a *Context call variant while a span is active
// additionally carry "trace_id" and "span_id" — see trace.go. Nothing
// needs configuring for that; with tracing disabled the attributes simply
// never appear.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// output is the swappable destination every component logger writes through.
// One handler is built over it at init and shared by all loggers, so SetOutput
// can redirect output even for loggers created as package-level vars before the
// redirect — which is what makes log output capturable in tests.
var output = &swapWriter{w: os.Stderr}

// processName is the process-scoped "proc" tag SetProcess installs. Package
// vars like `var routerLog = logging.Component("router")` run at package-init
// time, before main() has a chance to call SetProcess — so, like levelVar and
// output above, this has to be a live reference the shared handler re-reads
// per record (via procHandler.Handle), not a value baked into Component's
// call-time `.With(...)`. Empty until SetProcess is called; readers see "" as
// "no process tag" (see procHandler.Handle).
var processName atomic.Value

func init() {
	processName.Store("")
	levelVar.Set(ParseLevel(os.Getenv(envLevel)))
	slog.SetDefault(slog.New(procHandler{traceHandler{newHandler(output)}}))
}

// SetProcess binds a process-scoped "proc" attribute onto every subsequent
// log record from this binary — cap-broker vs. orchestrator legibility,
// at zero per-call-site cost: existing `component`-tagged loggers created
// before this call still pick it up, because procHandler injects it at
// Handle time rather than at Logger construction time. Call once, early
// at each entrypoint. Distinct from TF_ROLE (control/executor) — this
// names the privilege-separation component, not the horizontal-scaling
// role.
func SetProcess(name string) { processName.Store(name) }

// procHandler wraps the configured handler to inject the current proc
// attribute (if any) into every record, live — see processName's doc.
type procHandler struct{ slog.Handler }

func (h procHandler) Handle(ctx context.Context, r slog.Record) error {
	if p, _ := processName.Load().(string); p != "" {
		r.AddAttrs(slog.String("proc", p))
	}
	return h.Handler.Handle(ctx, r)
}

func (h procHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return procHandler{h.Handler.WithAttrs(attrs)}
}

func (h procHandler) WithGroup(name string) slog.Handler {
	return procHandler{h.Handler.WithGroup(name)}
}

// swapWriter is an io.Writer whose underlying destination can be replaced
// atomically at runtime.
type swapWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *swapWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	return w.Write(p)
}

func (s *swapWriter) swap(w io.Writer) io.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.w
	s.w = w
	return prev
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

// SetOutput redirects all log output to w and returns a function that restores
// the previous destination. Because component loggers share one handler over a
// swappable writer, this affects loggers already handed out by Component — so
// it is the way tests capture and assert on log records.
func SetOutput(w io.Writer) (restore func()) {
	prev := output.swap(w)
	return func() { output.swap(prev) }
}

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
