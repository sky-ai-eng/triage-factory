package worktree

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// sqliteHeader is a real SQLite file's first bytes plus a NUL run — enough for
// git to classify the path as binary, which is what forces the content diff
// down the `GIT binary patch` path (and, without --no-textconv, down whatever
// textconv the repo names for it). Shaped after the live capture failure, whose
// tree carried a stray `.triagefactory/triagefactory.db`.
var sqliteHeader = append([]byte("SQLite format 3\x00"), bytes.Repeat([]byte{0x00, 0x01, 0x02, 0xff}, 16)...)

// withCaptureChildEnv runs fn with this process's environment replaced by the
// real isolated-capture child environment, restoring the original afterwards.
//
// Production reaches git through two hops — the capture child gets
// sandbox.CaptureChildEnv(), and the git subprocesses it spawns inherit that via
// gitBaseEnv() — so replacing the process env wholesale is what puts an
// in-process CaptureWorkspaceGit under the same git config overrides the real
// child runs with. Nothing here stubs or paraphrases that env: the empty
// `diff.external` crash survived because no test ever combined the actual env
// with a tree that had changes to diff.
func withCaptureChildEnv(t *testing.T, fn func()) {
	t.Helper()
	saved := os.Environ()
	setAll := func(env []string) {
		os.Clearenv()
		for _, kv := range env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if err := os.Setenv(k, v); err != nil {
				t.Fatalf("setenv %s: %v", k, err)
			}
		}
	}
	defer setAll(saved)
	setAll(sandbox.CaptureChildEnv())
	fn()
}

// dirtyCaptureTree stands up a branch worktree holding a local-only commit and
// then dirties it the way a real parked run's tree is dirty: a modified tracked
// file, an untracked text file, and an untracked BINARY file under a
// dot-directory. The dot-directory matters — dotfiles sort first, so that path
// is the one a broken content diff dies on before it reaches anything else.
//
// Returns the worktree dir and its HEAD (the local-only commit, so the capture
// exercises the bundle path too).
func dirtyCaptureTree(t *testing.T, runID string) (wtDir, head string) {
	t.Helper()
	withTestHome(t)
	upstream := makeTestUpstream(t)
	wtDir, err := CreateForBranch(context.Background(), "acme", "repo", upstream, "main", "aa/feature", runID)
	if err != nil {
		t.Fatalf("CreateForBranch: %v", err)
	}
	t.Cleanup(func() { _ = RemoveAt(wtDir, runID) })

	gitAt(t, wtDir, "config", "user.email", "test@example.com")
	gitAt(t, wtDir, "config", "user.name", "Test")
	writeUnder(t, wtDir, "tracked.txt", "v1\n")
	gitAt(t, wtDir, "add", "-A")
	gitAt(t, wtDir, "commit", "-q", "-m", "agent commit")
	head = strings.TrimSpace(gitAt(t, wtDir, "rev-parse", "HEAD"))

	writeUnder(t, wtDir, "tracked.txt", "v1\nv2\n")
	writeUnder(t, wtDir, "untracked.txt", "new work\n")
	dbPath := filepath.Join(wtDir, ".triagefactory", "triagefactory.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir .triagefactory: %v", err)
	}
	if err := os.WriteFile(dbPath, sqliteHeader, 0o644); err != nil {
		t.Fatalf("write binary db: %v", err)
	}
	return wtDir, head
}

// plantSentinelDiffProgram writes a shell script that records every invocation
// into a sentinel file and prints a line of fake diff output, returning the
// script path and the sentinel path. Exiting 0 is deliberate: a program that
// failed would only prove the capture noticed it, while one that succeeds proves
// the capture ran it and swallowed its output as the file's contents — the
// silent-corruption shape.
func plantSentinelDiffProgram(t *testing.T) (script, sentinel string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "planted.sh")
	sentinel = filepath.Join(dir, "sentinel")
	body := "#!/bin/sh\necho ran >> " + sentinel + "\necho PLANTED PROGRAM OUTPUT\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write planted program: %v", err)
	}
	return script, sentinel
}

func assertNotRun(t *testing.T, sentinel string) {
	t.Helper()
	if b, err := os.ReadFile(sentinel); err == nil {
		t.Errorf("the planted program ran during capture (sentinel: %q)", b)
	}
}

// restoreAndAssertRoundTrip destroys wtDir and rebuilds it from the bare plus
// delta, then asserts the agent's work came back byte-for-byte — the property a
// patch produced through an external diff or a textconv would quietly break.
func restoreAndAssertRoundTrip(t *testing.T, wtDir string, delta *GitDelta) {
	t.Helper()
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	if err := RestoreWorkspaceGit(context.Background(), "acme", "repo", wtDir, delta, "", CloneAuth{}); err != nil {
		t.Fatalf("RestoreWorkspaceGit: %v", err)
	}
	assertFileContent(t, filepath.Join(wtDir, "tracked.txt"), "v1\nv2\n")
	assertFileContent(t, filepath.Join(wtDir, "untracked.txt"), "new work\n")
	got, err := os.ReadFile(filepath.Join(wtDir, ".triagefactory", "triagefactory.db"))
	if err != nil {
		t.Fatalf("read restored binary db: %v", err)
	}
	if !bytes.Equal(got, sqliteHeader) {
		t.Errorf("restored binary file differs from the captured one (%d bytes vs %d)", len(got), len(sqliteHeader))
	}
}

func assertCarriesAgentWork(t *testing.T, delta *GitDelta) {
	t.Helper()
	if delta == nil {
		t.Fatal("nil delta for a dirty worktree")
	}
	patch := string(delta.Patch)
	for _, want := range []string{"tracked.txt", "untracked.txt", ".triagefactory/triagefactory.db", "GIT binary patch"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}
}

// TestCaptureWorkspaceGit_DirtyTreeUnderCaptureChildEnv is the regression guard
// for the failure that shipped: under the real capture-child environment, whose
// empty `diff.external` git reads as an external diff command of "", the content
// diff aborted for EVERY changed path — so every snapshot of a dirty tree failed
// (park/resume handoff, step handoff, cross-executor rehydrate) while a clean
// tree, and every test that stubbed the child, sailed through.
func TestCaptureWorkspaceGit_DirtyTreeUnderCaptureChildEnv(t *testing.T) {
	wtDir, _ := dirtyCaptureTree(t, "capture-dirty-env-run")

	var delta *GitDelta
	withCaptureChildEnv(t, func() {
		var err error
		if delta, err = CaptureWorkspaceGit(context.Background(), wtDir); err != nil {
			t.Fatalf("CaptureWorkspaceGit under the real capture-child env: %v", err)
		}
	})
	assertCarriesAgentWork(t, delta)
	restoreAndAssertRoundTrip(t, wtDir, delta)
}

// TestCaptureWorkspaceGit_PlantedExternalDiffNeverRuns: the run root's own config
// is agent-writable and IS read by the capture, so `diff.external` there is the
// agent's to choose. The guarantee is --no-ext-diff at the invocation — which
// outranks config — not the absence of a value, so the capture must produce a
// correct patch without ever running what the agent named.
func TestCaptureWorkspaceGit_PlantedExternalDiffNeverRuns(t *testing.T) {
	wtDir, _ := dirtyCaptureTree(t, "capture-ext-diff-run")
	script, sentinel := plantSentinelDiffProgram(t)
	gitAt(t, wtDir, "config", "diff.external", script)

	var delta *GitDelta
	withCaptureChildEnv(t, func() {
		var err error
		if delta, err = CaptureWorkspaceGit(context.Background(), wtDir); err != nil {
			t.Fatalf("CaptureWorkspaceGit with a planted diff.external: %v", err)
		}
	})
	assertNotRun(t, sentinel)
	assertCarriesAgentWork(t, delta)
	if strings.Contains(string(delta.Patch), "PLANTED PROGRAM OUTPUT") {
		t.Errorf("patch carries the planted program's output:\n%s", delta.Patch)
	}
	restoreAndAssertRoundTrip(t, wtDir, delta)
}

// TestCaptureWorkspaceGit_PlantedTextconvNeverRuns covers the vector fixing the
// crash newly exposed: `git diff` is porcelain, so an attribute-mapped
// `diff.<driver>.textconv` runs by default — the crash was all that stood
// between the agent's `.gitattributes` and a program executing. It is equally a
// correctness bug: textconv output is not appliable, so a mapped binary path
// would round-trip as the converted TEXT, silently replacing the agent's file.
func TestCaptureWorkspaceGit_PlantedTextconvNeverRuns(t *testing.T) {
	wtDir, _ := dirtyCaptureTree(t, "capture-textconv-run")
	script, sentinel := plantSentinelDiffProgram(t)
	writeUnder(t, wtDir, ".gitattributes", "*.db diff=planted\n")
	gitAt(t, wtDir, "config", "diff.planted.textconv", script)

	var delta *GitDelta
	withCaptureChildEnv(t, func() {
		var err error
		if delta, err = CaptureWorkspaceGit(context.Background(), wtDir); err != nil {
			t.Fatalf("CaptureWorkspaceGit with a planted textconv: %v", err)
		}
	})
	assertNotRun(t, sentinel)
	assertCarriesAgentWork(t, delta)
	if strings.Contains(string(delta.Patch), "PLANTED PROGRAM OUTPUT") {
		t.Errorf("patch carries the planted program's output:\n%s", delta.Patch)
	}
	restoreAndAssertRoundTrip(t, wtDir, delta)
}

// TestCaptureUncommitted_EveryDiffForbidsDriverExec pins the flags on the
// invocations themselves, source-level. The behavioral tests above can only
// cover a diff that actually invokes a driver, so a `--name-only` diff — or any
// diff added later — could lose the flags with every other test still green.
// Reading the source is the only check that sees that.
func TestCaptureUncommitted_EveryDiffForbidsDriverExec(t *testing.T) {
	src, err := os.ReadFile("snapshot.go")
	if err != nil {
		t.Fatalf("read snapshot.go: %v", err)
	}
	found := 0
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, `gitCapture(`) || !strings.Contains(line, `"diff"`) {
			continue
		}
		found++
		for _, flag := range []string{"--no-ext-diff", "--no-textconv"} {
			if !strings.Contains(line, flag) {
				t.Errorf("diff invocation lacks %s (an agent-writable config could name the program it runs):\n\t%s", flag, strings.TrimSpace(line))
			}
		}
	}
	if found == 0 {
		t.Fatal("found no gitCapture diff invocation in snapshot.go; this guard has stopped guarding anything")
	}
}
