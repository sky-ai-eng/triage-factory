//go:build linux && integration

// Integration coverage for the TFAC-61 shared read-only pinned-repo mount
// model. Build-tagged + skip-guarded exactly like integration_linux_test.go:
// `go test ./...` skips it; the sandbox-CI box (runsc + root) runs it via
// `go test -tags integration`.

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegration_SharedRORepoMount drives the Curator's multi-mode mount
// model through a real runsc jail and asserts the three security-critical
// runtime properties:
//
//   - read works: a shared worktree bind-mounted ro at /work/repos/<o>/<r> is
//     readable by the sandbox UID (no chown of the shared tree — it's left
//     world-readable as a git checkout under umask 022 is), so the agent reads
//     its pinned repos with no missing-mount errors.
//   - write blocked: an in-jail write to that mount fails (the ro option is the
//     boundary), so one session cannot mutate what another reads.
//   - no escape: a sibling shared tree that is NOT mounted is unreachable from
//     inside the jail — nothing outside the declared mount set is exposed.
func TestIntegration_SharedRORepoMount(t *testing.T) {
	requireRunsc(t)

	// The shared worktree: a host dir OUTSIDE the cwd, left owned by the host
	// user (no per-session chown) and made world-readable like a git checkout
	// under umask 022 — t.TempDir() is 0700, so model production explicitly.
	sharedWt := t.TempDir()
	if err := os.Chmod(sharedWt, 0o755); err != nil {
		t.Fatalf("chmod shared worktree: %v", err)
	}
	const sentinel = "shared-ro-readable-sentinel"
	if err := os.WriteFile(filepath.Join(sharedWt, "README.md"), []byte(sentinel+"\n"), 0o644); err != nil {
		t.Fatalf("write shared file: %v", err)
	}

	// A second shared tree for a different repo that this jail does NOT mount —
	// the no-escape probe.
	otherWt := t.TempDir()
	if err := os.Chmod(otherWt, 0o755); err != nil {
		t.Fatalf("chmod other worktree: %v", err)
	}
	const otherSentinel = "unmounted-must-not-be-reachable"
	if err := os.WriteFile(filepath.Join(otherWt, "secret"), []byte(otherSentinel+"\n"), 0o644); err != nil {
		t.Fatalf("write other file: %v", err)
	}

	newCfg := func(t *testing.T, tag string) Config {
		cfg := minimalConfig(t) // chowns cwd to UID 10000; skips if not root
		cfg.RunID = "itest-tfac61-" + tag
		// Empty mount point under the cwd (the /work mount), exactly as
		// agentproc.ensureRepoMountPoints creates it; the ro bind overlays the
		// shared worktree here.
		mp := filepath.Join(cfg.Worktree, "repos", "o", "r")
		if err := os.MkdirAll(mp, 0o755); err != nil {
			t.Fatalf("mkdir mount point: %v", err)
		}
		cfg.ExtraMounts = []Mount{
			{Source: sharedWt, Destination: "/work/repos/o/r", Options: []string{"ro"}},
		}
		return cfg
	}

	run := func(t *testing.T, cfg Config) string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		run, sb, err := Wrap(ctx, cfg)
		if err != nil {
			t.Fatalf("Wrap: %v", err)
		}
		defer sb.Close()
		defer run.Close()
		out, _ := runToCompletion(t, run)
		return string(out)
	}

	t.Run("read works", func(t *testing.T) {
		cfg := newCfg(t, "read")
		cfg.Argv = []string{"/bin/cat", "/work/repos/o/r/README.md"}
		if out := run(t, cfg); !strings.Contains(out, sentinel) {
			t.Errorf("shared ro mount not readable in jail; output: %s", out)
		}
	})

	t.Run("write blocked", func(t *testing.T) {
		cfg := newCfg(t, "write")
		// Echo a marker only on a successful write, so a broken ro guarantee is
		// detectable rather than silently swallowed.
		cfg.Argv = []string{"/bin/sh", "-c", "echo x > /work/repos/o/r/evil && echo WROTE || echo BLOCKED"}
		out := run(t, cfg)
		if strings.Contains(out, "WROTE") {
			t.Errorf("ro mount is writable in jail — in-jail write must fail; output: %s", out)
		}
		if !strings.Contains(out, "BLOCKED") {
			t.Errorf("expected BLOCKED marker (write rejected); output: %s", out)
		}
		if _, err := os.Stat(filepath.Join(sharedWt, "evil")); err == nil {
			t.Errorf("in-jail write reached the host shared tree — ro mount breached")
		}
	})

	t.Run("unmounted tree unreachable", func(t *testing.T) {
		cfg := newCfg(t, "escape")
		// otherWt is a host path that is not bind-mounted, so it does not exist
		// inside the jail (mirrors TestIntegration_WorktreeIsolation).
		cfg.Argv = []string{"/bin/cat", filepath.Join(otherWt, "secret")}
		if out := run(t, cfg); strings.Contains(out, otherSentinel) {
			t.Errorf("unmounted host tree reachable from jail — escape; output: %s", out)
		}
	})
}
