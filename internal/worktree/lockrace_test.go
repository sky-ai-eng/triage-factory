package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// TestIsWorktreeAddLockRace_MatchesOnlyTheNarrowSignature pins the matcher
// gitRunCtxAuth's retry gates on: it must fire only for `worktree add`
// hitting exactly the transient locked-marker error, never for a
// look-alike message or a different subcommand — a broad match would risk
// silently retrying (and thereby masking) a real failure like a bad ref or
// an already-registered worktree.
func TestIsWorktreeAddLockRace_MatchesOnlyTheNarrowSignature(t *testing.T) {
	raceMsg := []byte("Preparing worktree (detached HEAD deadbeef)\nfatal: could not open 'worktrees/r/locked' for writing: No such file or directory\n")
	tests := []struct {
		name string
		args []string
		out  []byte
		want bool
	}{
		{"worktree add + exact race text", []string{"worktree", "add", "--detach", "/wt", "HEAD"}, raceMsg, true},
		{"worktree add -B variant + exact race text", []string{"worktree", "add", "-B", "main", "/wt", "HEAD"}, raceMsg, true},
		{"different subcommand (fetch) with same text", []string{"fetch", "origin"}, raceMsg, false},
		{"worktree prune (not add)", []string{"worktree", "prune"}, raceMsg, false},
		{"worktree add, unrelated failure", []string{"worktree", "add", "/wt", "HEAD"}, []byte("fatal: '/wt' already exists"), false},
		{"worktree add, already registered", []string{"worktree", "add", "/wt", "HEAD"}, []byte("fatal: '/wt' is already registered as a linked worktree"), false},
		{"too few args", []string{"worktree"}, raceMsg, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorktreeAddLockRace(tc.args, tc.out); got != tc.want {
				t.Errorf("isWorktreeAddLockRace(%v, %q) = %v, want %v", tc.args, tc.out, got, tc.want)
			}
		})
	}
}

// writeFakeGit installs a shim named "git" at the front of $PATH that
// intercepts `worktree add` invocations: the first failCount calls report
// the exact transient locked-marker error observed in CI, then it delegates
// to the real git (recorded once via exec.LookPath before PATH is
// overridden) for that call and every other subcommand. attempts, read
// after the test, is the number of `worktree add` invocations the shim saw.
func writeFakeGit(t *testing.T, failCount int) (attempts func() int) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find real git: %v", err)
	}
	fakebin := t.TempDir()
	counterFile := filepath.Join(t.TempDir(), "attempts")

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "worktree" ] && [ "$2" = "add" ]; then
  n=$(cat %q 2>/dev/null || echo 0)
  n=$((n + 1))
  echo "$n" > %q
  if [ "$n" -le %d ]; then
    echo "Preparing worktree (detached HEAD deadbeef)" 1>&2
    echo "fatal: could not open 'worktrees/r/locked' for writing: No such file or directory" 1>&2
    exit 128
  fi
fi
exec %q "$@"
`, counterFile, counterFile, failCount, realGit)
	if err := os.WriteFile(filepath.Join(fakebin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}

	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() int {
		data, err := os.ReadFile(counterFile)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(string(data[:len(data)-1])) // trim trailing newline
		return n
	}
}

// TestGitRunCtxAuth_RetriesAndRecoversFromLockRace drives gitRunCtxAuth
// against a `worktree add` that fails with the transient locked-marker
// signature exactly once, then succeeds — the shape CI hit. The retry must
// absorb it silently and return success.
func TestGitRunCtxAuth_RetriesAndRecoversFromLockRace(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	bareDir, err := EnsureBareClone(context.Background(), "acme", "retry-ok", upstream)
	if err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	attempts := writeFakeGit(t, worktreeAddLockRaceMaxAttempts-1)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := gitRunCtxAuth(context.Background(), bareDir, CloneAuth{}, "worktree", "add", "--detach", wt, "main"); err != nil {
		t.Fatalf("worktree add: %v, want success after retrying the transient race", err)
	}
	if got, want := attempts(), worktreeAddLockRaceMaxAttempts; got != want {
		t.Errorf("shim saw %d worktree-add invocations, want %d (fail %d times then succeed)", got, want, worktreeAddLockRaceMaxAttempts-1)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Errorf("expected a real checkout at %s after the retried add succeeded: %v", wt, err)
	}
}

// TestGitRunCtxAuth_GivesUpAfterMaxAttempts is the bound on the retry: a
// persistently failing `worktree add` (not just a one-off blip) must still
// surface as an error rather than retrying forever.
func TestGitRunCtxAuth_GivesUpAfterMaxAttempts(t *testing.T) {
	withTestHome(t)
	upstream := makeTestUpstream(t)
	bareDir, err := EnsureBareClone(context.Background(), "acme", "retry-fail", upstream)
	if err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	attempts := writeFakeGit(t, worktreeAddLockRaceMaxAttempts+5)
	wt := filepath.Join(t.TempDir(), "wt")
	err = gitRunCtxAuth(context.Background(), bareDir, CloneAuth{}, "worktree", "add", "--detach", wt, "main")
	if err == nil {
		t.Fatal("expected an error once the race outlasts the retry budget")
	}
	if got, want := attempts(), worktreeAddLockRaceMaxAttempts; got != want {
		t.Errorf("shim saw %d worktree-add invocations, want exactly %d (the retry cap)", got, want)
	}
}
