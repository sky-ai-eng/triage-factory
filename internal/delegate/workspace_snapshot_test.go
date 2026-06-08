package delegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestEnsureWorkspace_WarmPath_NoRehydrate is the warm-path acceptance: a run
// that parked keeps its worktree on disk, so ensureWorkspace returns it as-is
// and does NOT rebuild from the snapshot. A marker file written AFTER the
// snapshot survives — proof the warm copy was used, not a rehydrate that would
// have lost it.
func TestEnsureWorkspace_WarmPath_NoRehydrate(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-warm"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	const sessionID = "sess-warm"
	writeSession(t, wtPath, sessionID, `{"type":"summary"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrg, runID, wtPath, sessionID); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Written after the snapshot: a rehydrate rebuilds from the (older) blob and
	// would lose this, so its survival distinguishes warm reuse from a rebuild.
	marker := filepath.Join(wtPath, "_scratch", "ci-logs", "warm-marker.txt")
	writeFile(t, marker, "warm")

	run := &domain.AgentRun{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrg, run, owner, repo, "")
	if err != nil {
		t.Fatalf("ensureWorkspace (warm): %v", err)
	}
	if got != wtPath {
		t.Errorf("warm cwd = %q, want the on-disk worktree %q", got, wtPath)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("post-snapshot marker missing — ensureWorkspace rehydrated instead of reusing the warm copy: %v", err)
	}
}

// TestEnsureWorkspace_ColdPath_RehydratesFromSnapshot is the cold-path
// acceptance: a parked run whose local worktree (and session JSONL) are then
// lost — simulating host loss / a /tmp wipe — resumes by rebuilding from the
// snapshot. The agent's committed work (carried in the git bundle), the
// uncommitted changes (the patch), the ephemeral _scratch (minus the
// re-materializable subdirs), and an intact `--resume` session must all be
// restored.
func TestEnsureWorkspace_ColdPath_RehydratesFromSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-cold"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	// The agent commits work (advances the branch; rides in the bundle).
	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")

	// ...then leaves an uncommitted edit (rides in the patch).
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nuncommitted edit\n")

	// Ephemeral _scratch is snapshotted; entity-memory / project-knowledge are
	// excluded (they re-materialize from the DB / project KB).
	writeFile(t, filepath.Join(wtPath, "_scratch", "ci-logs", "build.log"), "ci log line")
	writeFile(t, filepath.Join(wtPath, "_scratch", "entity-memory", "ns", "x.md"), "memory")
	writeFile(t, filepath.Join(wtPath, "_scratch", "project-knowledge", "kb.md"), "kb")

	const sessionID = "sess-cold"
	sessPath := writeSession(t, wtPath, sessionID, `{"type":"summary","sid":"cold"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrg, runID, wtPath, sessionID); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Simulate host loss / /tmp wipe: the worktree and session JSONL are gone,
	// and the bare no longer has the agent's local branch (a fresh clone only
	// carries the remote) — so the committed state can ONLY come back via the
	// bundle. The persistent bare itself survives (state-root, not /tmp).
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	if err := os.Remove(sessPath); err != nil {
		t.Fatalf("rm session: %v", err)
	}
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	gitT(t, bareDir, "worktree", "prune")
	gitT(t, bareDir, "branch", "-D", "feature")

	run := &domain.AgentRun{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrg, run, owner, repo, "")
	if err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}
	if got != wtPath {
		t.Errorf("rehydrated cwd = %q, want %q", got, wtPath)
	}

	assertFileContains(t, filepath.Join(got, "agent.txt"), "committed by agent") // bundle
	assertFileContains(t, filepath.Join(got, "README.md"), "uncommitted edit")   // patch
	assertFileContains(t, filepath.Join(got, "_scratch", "ci-logs", "build.log"), "ci log line")
	assertMissing(t, filepath.Join(got, "_scratch", "entity-memory", "ns", "x.md"))
	assertMissing(t, filepath.Join(got, "_scratch", "project-knowledge", "kb.md"))

	// The session transcript lands under the rebuilt cwd's encoded project dir
	// so `claude --resume` reconnects.
	sessPath2, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(got), sessionID)
	if err != nil {
		t.Fatalf("ClaudeSessionPath: %v", err)
	}
	assertFileContains(t, sessPath2, `"sid":"cold"`)
}

// TestEnsureWorkspace_ColdPath_DetachedHead: a worktree snapshotted while HEAD
// is detached (e.g. the agent ran `git checkout <sha>`) rehydrates to the same
// detached commit — the manifest carries the HEAD SHA, so an empty branch no
// longer strands the run.
func TestEnsureWorkspace_ColdPath_DetachedHead(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-detached"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	// Agent commits, then detaches HEAD at that commit.
	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")
	gitT(t, wtPath, "checkout", "--detach", "HEAD")
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\ndetached edit\n")
	headSHA := strings.TrimSpace(gitOut(t, wtPath, "rev-parse", "HEAD"))

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrg, runID, wtPath, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Lose the worktree and drop the branch so the commit returns via the bundle.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	gitT(t, bareDir, "worktree", "prune")
	gitT(t, bareDir, "branch", "-D", "feature")

	run := &domain.AgentRun{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrg, run, owner, repo, "")
	if err != nil {
		t.Fatalf("ensureWorkspace (detached): %v", err)
	}
	assertFileContains(t, filepath.Join(got, "agent.txt"), "committed by agent")
	assertFileContains(t, filepath.Join(got, "README.md"), "detached edit")
	if gotSHA := strings.TrimSpace(gitOut(t, got, "rev-parse", "HEAD")); gotSHA != headSHA {
		t.Errorf("rehydrated HEAD = %s, want %s", gotSHA, headSHA)
	}
	if err := exec.Command("git", "-C", got, "symbolic-ref", "-q", "HEAD").Run(); err == nil {
		t.Error("rehydrated HEAD is on a branch; want detached")
	}
}

// TestEnsureWorkspace_ColdPath_NeverPushedBranchNoCommits: a run on a local
// branch that was never pushed and has no local-only commits (only uncommitted
// changes) still rehydrates — the manifest's HEAD SHA recreates the branch even
// though the bundle is empty and there's no refs/remotes/origin/<branch>.
func TestEnsureWorkspace_ColdPath_NeverPushedBranchNoCommits(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-nopush"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	// No commits — only an uncommitted edit on the never-pushed "feature" branch.
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nwork in progress\n")
	headSHA := strings.TrimSpace(gitOut(t, wtPath, "rev-parse", "HEAD"))

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrg, runID, wtPath, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Simulate a fresh host: worktree gone and the bare lacks the never-pushed
	// branch (only origin/* survives a fresh clone), but it still has origin/main
	// — which is where HEAD points, since "feature" had no commits.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	gitT(t, bareDir, "worktree", "prune")
	gitT(t, bareDir, "branch", "-D", "feature")

	run := &domain.AgentRun{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrg, run, owner, repo, "")
	if err != nil {
		t.Fatalf("ensureWorkspace (never-pushed branch): %v", err)
	}
	assertFileContains(t, filepath.Join(got, "README.md"), "work in progress")
	if gotBranch := strings.TrimSpace(gitOut(t, got, "rev-parse", "--abbrev-ref", "HEAD")); gotBranch != "feature" {
		t.Errorf("rehydrated branch = %q, want feature", gotBranch)
	}
	if gotSHA := strings.TrimSpace(gitOut(t, got, "rev-parse", "HEAD")); gotSHA != headSHA {
		t.Errorf("rehydrated HEAD = %s, want %s", gotSHA, headSHA)
	}
}

// TestEnsureWorkspace_ColdPath_NoSnapshotErrors: with the worktree gone and no
// snapshot ever written, ensureWorkspace surfaces a clear error rather than
// silently handing back a dead path.
func TestEnsureWorkspace_ColdPath_NoSnapshotErrors(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	run := &domain.AgentRun{ID: "wt-missing", WorktreePath: filepath.Join(t.TempDir(), "gone")}
	if _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrg, run, "o", "r", ""); err == nil {
		t.Fatal("ensureWorkspace should error when neither the worktree nor a snapshot exists")
	}
}

// TestFailRun_DiscardsWorkspaceSnapshot: a parked run that then fails (e.g. an
// open run whose resume errors mid-execution) drops its snapshot rather than
// orphaning the blob. failRun is the single failure chokepoint covering the
// resume goroutine's failure exits.
func TestFailRun_DiscardsWorkspaceSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, _, runID, taskID := setupAdvanceFixture(t, "failrun-discard")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)

	ctx := context.Background()
	key := snapshotKey(runmode.LocalDefaultOrg, runID)
	if err := blobs.Put(ctx, key, strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// triggerType "event" so failRun routes through the admin-pool System
	// methods (no synthetic-claims tx needed in the fixture).
	s.failRun(runmode.LocalDefaultOrg, runID, taskID, "event", "", "boom")

	if ok, _ := blobs.Exists(ctx, key); ok {
		t.Error("failRun did not discard the workspace snapshot — blob orphaned on failure")
	}
}

// --- helpers ---------------------------------------------------------------

// newStorageSpawner builds a bare Spawner with only the blob store wired —
// enough for the snapshot/rehydrate path, which touches no DB on the same host
// (the rebuilt cwd equals the stored worktree_path, so no SetWorktreePath).
func newStorageSpawner(t *testing.T) *Spawner {
	t.Helper()
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	s.SetStorage(blobs)
	return s
}

// setupTestWorktree stands up a real origin bare + a delegated worktree on a
// "feature" branch via the production worktree path, so the snapshot/rehydrate
// code exercises actual git plumbing. Returns the worktree path and its
// owner/repo.
func setupTestWorktree(t *testing.T, runID string) (wtPath, owner, repo string) {
	t.Helper()
	owner, repo = "o", "r"

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, "", "init", "--bare", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	gitT(t, "", "clone", origin, seed)
	gitT(t, seed, "checkout", "-b", "main")
	writeFile(t, filepath.Join(seed, "README.md"), "hello\n")
	gitT(t, seed, "add", "README.md")
	gitT(t, seed, "commit", "-m", "init")
	gitT(t, seed, "push", "origin", "main")

	wt, err := worktree.CreateForBranch(context.Background(), owner, repo, origin, "main", "feature", runID)
	if err != nil {
		t.Fatalf("CreateForBranch: %v", err)
	}
	return wt, owner, repo
}

// setupGitTestEnv isolates HOME (so ~/.claude session writes + git config land
// in a throwaway dir) and pins a git identity so commits don't depend on the
// developer's global config.
func setupGitTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// gitOut runs git and returns stdout (for rev-parse and friends).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %q: %v", strings.Join(args, " "), dir, err)
	}
	return string(out)
}

// writeSession writes a fake Claude session transcript for wtPath's cwd and
// returns its path.
func writeSession(t *testing.T, wtPath, sessionID, body string) string {
	t.Helper()
	p, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(wtPath), sessionID)
	if err != nil {
		t.Fatalf("ClaudeSessionPath: %v", err)
	}
	writeFile(t, p, body)
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("%s = %q, want it to contain %q", path, string(data), want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists or errored unexpectedly (%v); it should have been excluded from the snapshot", path, err)
	}
}
