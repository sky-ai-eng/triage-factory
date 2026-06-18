package agentproc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestClaudeBinaryOverride(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	// Unset → no override, no error.
	t.Run("unset", func(t *testing.T) {
		t.Setenv(envClaudeBinary, "")
		got, err := claudeBinaryOverride()
		if err != nil || got != "" {
			t.Fatalf("unset: got (%q, %v), want (\"\", nil)", got, err)
		}
	})

	// A valid executable file → its absolute path.
	t.Run("valid executable", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "claude")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envClaudeBinary, bin)
		got, err := claudeBinaryOverride()
		if err != nil {
			t.Fatalf("valid: unexpected error %v", err)
		}
		if got != bin {
			t.Errorf("valid: got %q, want %q", got, bin)
		}
	})

	// Missing path → hard error (don't silently fall back).
	t.Run("missing", func(t *testing.T) {
		t.Setenv(envClaudeBinary, filepath.Join(t.TempDir(), "nope"))
		if _, err := claudeBinaryOverride(); err == nil {
			t.Error("missing path should error")
		}
	})

	// A directory → error.
	t.Run("directory", func(t *testing.T) {
		t.Setenv(envClaudeBinary, t.TempDir())
		if _, err := claudeBinaryOverride(); err == nil {
			t.Error("directory should error")
		}
	})

	// A non-executable file → error.
	t.Run("not executable", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "claude")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envClaudeBinary, f)
		if _, err := claudeBinaryOverride(); err == nil {
			t.Error("non-executable file should error")
		}
	})

	// Multi mode ignores the override entirely — even a valid binary — so it
	// can't undercut the image-baked binary regardless of sandbox routing.
	t.Run("multi mode ignored", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		dir := t.TempDir()
		bin := filepath.Join(dir, "claude")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envClaudeBinary, bin)
		got, err := claudeBinaryOverride()
		if err != nil || got != "" {
			t.Fatalf("multi mode: got (%q, %v), want (\"\", nil)", got, err)
		}
	})
}
