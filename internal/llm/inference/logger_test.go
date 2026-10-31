package inference

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// TestBifrostLogger_RoutesToSlog pins that embedded bifrost's diagnostics land
// in TF's log stream rather than in a second format on stdout, where they
// carry no component or proc tag and cannot be tied to the run that failed.
func TestBifrostLogger_RoutesToSlog(t *testing.T) {
	var buf bytes.Buffer
	restore := logging.SetOutput(&buf)
	defer restore()
	logging.SetLevel(slog.LevelDebug)
	defer logging.SetLevel(logging.ParseLevel(""))

	log := newBifrostLogger()
	log.Warn("provider %s returned %d", "anthropic", 502)

	out := buf.String()
	for _, want := range []string{"provider anthropic returned 502", "component=bifrost"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output %q is missing %q", out, want)
		}
	}
}

// TestBifrostLogger_FatalDoesNotExit pins the one inherited behavior that
// would be unrecoverable: bifrost's own Fatal ends in os.Exit, which would
// take the executor down mid-run along with every other conversation on it.
// The test reaching its final line IS the assertion.
func TestBifrostLogger_FatalDoesNotExit(t *testing.T) {
	var buf bytes.Buffer
	restore := logging.SetOutput(&buf)
	defer restore()

	newBifrostLogger().Fatal("unrecoverable: %v", "boom")

	if !strings.Contains(buf.String(), "unrecoverable: boom") {
		t.Fatalf("a fatal must still be logged, at error: %q", buf.String())
	}
}

// TestBifrostLogger_SetLevelIsInert pins that bifrost cannot reach past its
// own logger and change the level for the whole process — its implementation
// calls zerolog.SetGlobalLevel.
func TestBifrostLogger_SetLevelIsInert(t *testing.T) {
	before := logging.Level()
	newBifrostLogger().SetLevel(schemas.LogLevelError)
	if after := logging.Level(); after != before {
		t.Fatalf("level moved from %v to %v", before, after)
	}
}

// TestBifrostLogger_FiltersLifecycleChatter pins the demotion where it
// actually happens. bifrost calls its logger unconditionally and relied on
// zerolog inside the default implementation to drop what the level excluded,
// so overriding those methods removes the only filter in the path — and the
// loop constructs a client per provider call, making info-tier lifecycle
// output two lines of nothing between every turn and the next.
func TestBifrostLogger_FiltersLifecycleChatter(t *testing.T) {
	var buf bytes.Buffer
	restore := logging.SetOutput(&buf)
	defer restore()
	logging.SetLevel(slog.LevelInfo)
	defer logging.SetLevel(logging.ParseLevel(""))

	log := newBifrostLogger()
	log.Info("closing all request channels...")
	log.Warn("provider is overloaded")

	out := buf.String()
	if strings.Contains(out, "closing all request channels") {
		t.Errorf("per-call lifecycle chatter must not reach TF's default level: %q", out)
	}
	if !strings.Contains(out, "provider is overloaded") {
		t.Errorf("warnings must always pass: %q", out)
	}

	// Debug opens it back up, which is the point of demoting rather than dropping.
	buf.Reset()
	logging.SetLevel(slog.LevelDebug)
	log.Info("closing all request channels...")
	if !strings.Contains(buf.String(), "closing all request channels") {
		t.Errorf("TF_LOG_LEVEL=debug must surface bifrost's info tier: %q", buf.String())
	}
}

// TestBifrostLevel pins the deliberate asymmetry: TF's default level must not
// let bifrost's per-call lifecycle chatter through, but debug must.
func TestBifrostLevel(t *testing.T) {
	for in, want := range map[slog.Level]schemas.LogLevel{
		slog.LevelDebug - 4: schemas.LogLevelDebug,
		slog.LevelDebug:     schemas.LogLevelDebug,
		slog.LevelInfo:      schemas.LogLevelWarn,
		slog.LevelWarn:      schemas.LogLevelWarn,
		slog.LevelError:     schemas.LogLevelError,
		slog.LevelError + 4: schemas.LogLevelError,
	} {
		if got := bifrostLevel(in); got != want {
			t.Errorf("bifrostLevel(%v) = %v, want %v", in, got, want)
		}
	}
}
