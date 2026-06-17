package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestComponentTagsRecords verifies Component stamps the component attribute
// and that SetLevel filters live on an already-handed-out logger.
func TestComponentTagsRecords(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: levelVar})))

	log := Component("router")

	SetLevel(slog.LevelInfo)
	log.Debug("filtered out")
	if buf.Len() != 0 {
		t.Fatalf("debug record emitted at info level: %q", buf.String())
	}

	log.Info("task bumped", "task", "T1", "events", 3)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode record: %v (%q)", err, buf.String())
	}
	if rec["component"] != "router" {
		t.Errorf("component = %v, want router", rec["component"])
	}
	if rec["msg"] != "task bumped" || rec["task"] != "T1" {
		t.Errorf("unexpected record: %v", rec)
	}

	// SetLevel raises the bar live: the same logger now drops Info.
	buf.Reset()
	SetLevel(slog.LevelError)
	log.Info("now filtered")
	if buf.Len() != 0 {
		t.Fatalf("info record emitted at error level: %q", buf.String())
	}
	SetLevel(slog.LevelInfo)
}

func TestSetOutputRedirects(t *testing.T) {
	var buf bytes.Buffer
	restore := SetOutput(&buf)

	// A component logger created here writes through the shared swappable
	// writer, so SetOutput must capture it.
	Component("widget").Info("hello", "k", "v")
	out := buf.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "component=widget") {
		t.Fatalf("SetOutput did not capture component log: %q", out)
	}

	// After restore, the buffer no longer receives records.
	restore()
	buf.Reset()
	Component("widget").Info("after restore")
	if buf.Len() != 0 {
		t.Fatalf("output still captured after restore: %q", buf.String())
	}
}
