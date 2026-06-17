package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
