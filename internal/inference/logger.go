package inference

import (
	"context"
	"fmt"
	"log/slog"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// bifrostLog is where embedded bifrost's own diagnostics land. Without this
// adapter they go to zerolog on stdout in a second format with no component
// or proc tag — invisible to anyone grepping TF's logs for the run that
// failed, and unfilterable by TF_LOG_LEVEL.
var bifrostLog = logging.Component("bifrost")

// slogLogger routes bifrost's logger onto TF's slog. The embedded
// *bifrost.DefaultLogger supplies the parts of the interface with no slog
// equivalent (the LogHTTPRequest event builder, the output-type setter), so
// only the five leveled methods and SetLevel are overridden here.
//
// Two behaviors are deliberately not inherited:
//
//   - SetLevel is a no-op. bifrost's implementation calls
//     zerolog.SetGlobalLevel, which reaches past its own logger and changes
//     logging for the whole process. TF's level is TF_LOG_LEVEL's to set.
//   - Fatal logs at error and returns. bifrost's implementation ends in
//     zerolog's Fatal, which calls os.Exit — a library holding the power to
//     kill the executor mid-run, taking every other conversation on the pod
//     with it. Nothing in bifrost core calls it today; this makes sure that
//     staying true isn't load-bearing.
type slogLogger struct {
	*bifrost.DefaultLogger
	log *slog.Logger
}

// newBifrostLogger builds the adapter. The embedded default logger is
// constructed at the matching level for consistency, but it is not what
// filters: bifrost calls its logger's methods unconditionally and relied on
// zerolog inside them to drop what the level excluded. Overriding those
// methods takes that filter out of the path, so emit reimplements it.
func newBifrostLogger() schemas.Logger {
	return &slogLogger{
		DefaultLogger: bifrost.NewDefaultLogger(bifrostLevel(logging.Level())),
		log:           bifrostLog,
	}
}

// bifrost's leveled methods are printf-style (its default logger ends in
// zerolog's Msgf), so args format the message rather than forming key/value
// pairs. Rendering them through slog's structured API would print the
// verb-laden template and drop the values.

func (l *slogLogger) Debug(msg string, args ...any) { l.emit(slog.LevelDebug, msg, args) }
func (l *slogLogger) Info(msg string, args ...any)  { l.emit(slog.LevelInfo, msg, args) }
func (l *slogLogger) Warn(msg string, args ...any)  { l.emit(slog.LevelWarn, msg, args) }
func (l *slogLogger) Error(msg string, args ...any) { l.emit(slog.LevelError, msg, args) }
func (l *slogLogger) Fatal(msg string, args ...any) { l.emit(slog.LevelError, msg, args) }

// emit gates on TF's live level rather than one captured at construction, so
// a level change takes effect on a client already mid-engagement — the same
// property logging.Component's own loggers have.
func (l *slogLogger) emit(level slog.Level, msg string, args []any) {
	if level < bifrostFloor(logging.Level()) {
		return
	}
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	l.log.Log(context.Background(), level, msg)
}

// SetLevel is deliberately inert — see the type doc.
func (l *slogLogger) SetLevel(schemas.LogLevel) {}

// bifrostFloor is the lowest level bifrost's own records are emitted at, given
// TF's level. It is TF's level, one notch quieter at the info end: bifrost's
// info tier is worker-pool lifecycle ("closing all request channels..."), and
// the loop builds and tears down a client on every single provider call, so
// passing it through at TF's default level would put two lines of nothing
// between every turn and the next. Warnings and errors — the half worth
// having — pass at full level, and TF_LOG_LEVEL=debug opens it all up.
func bifrostFloor(tf slog.Level) slog.Level {
	if tf > slog.LevelDebug && tf < slog.LevelWarn {
		return slog.LevelWarn
	}
	return tf
}

// bifrostLevel maps a floor onto bifrost's own vocabulary, which has no tier
// below debug and none above error — so both ends clamp.
func bifrostLevel(l slog.Level) schemas.LogLevel {
	switch floor := bifrostFloor(l); {
	case floor <= slog.LevelDebug:
		return schemas.LogLevelDebug
	case floor <= slog.LevelInfo:
		return schemas.LogLevelInfo
	case floor <= slog.LevelWarn:
		return schemas.LogLevelWarn
	default:
		return schemas.LogLevelError
	}
}
