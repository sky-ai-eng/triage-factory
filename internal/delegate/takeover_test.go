package delegate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// These tests cover the validation paths Takeover() walks BEFORE it
// touches the worktree or DB row state. They don't need a real claude
// subprocess or git repo — Takeover bails out early on any of them
// with a typed sentinel error, and the HTTP handler maps those
// sentinels to status codes.
//
// The post-validation paths (copy, mark, abortTakeover) are exercised
// by the worktree and DB suites because they're the ones with
// non-trivial state machines; here we just guard the early-return
// contract the handler depends on.

// newTakeoverTestDB spins up an in-memory SQLite with the full schema
// so we can seed runs in any state Takeover validation cares about.
// Forcing single-conn because :memory: is per-conn — a pooled second
// connection would see an empty schema.
func newTakeoverTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// seedRunBlueprint mints a blueprint + blueprint_run for taskID and returns the
// blueprint_run id, so run fixtures can satisfy runs.blueprint_run_id NOT NULL.
// The suffix keeps ids unique + deterministic per fixture; the ids are distinct
// from makeRunBlueprintStep's ("bp-"/"bpr-") so a test can re-point a run onto a
// specific blueprint_run without colliding with the one seedRun attached.
func seedRunBlueprint(t *testing.T, database *sql.DB, suffix, taskID string) string {
	t.Helper()
	bpID := "seedbp-" + suffix
	if err := sqlitestore.New(database).Blueprints.Create(context.Background(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := "seedbpr-" + suffix
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan) VALUES (?, ?, ?, 'manual', ?, '[]')`,
		brID, bpID, taskID, "/tmp/wt-"+brID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedRun inserts a run with the requested fields and returns its ID.
// We bypass the spawner's Delegate flow because the validation tests
// don't need a real goroutine — only a row in the runs table. Every run is a
// blueprint step now (runs.blueprint_run_id NOT NULL), so it mints a 1-step
// blueprint_run and links the run to it.
func seedRun(t *testing.T, database *sql.DB, runID, sessionID, worktreePath string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#"+runID, "pr", "T", "https://example.com/"+runID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, runID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedRunBlueprint(t, database, runID, task.ID)
	stepIdx := 0
	if err := sqlitestore.New(database).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, domain.AgentRun{
		ID:                 runID,
		TaskID:             task.ID,
		PromptID:           "test-prompt",
		Status:             "running",
		Model:              "claude-sonnet-4-6",
		WorktreePath:       worktreePath,
		BlueprintRunID:     brID,
		BlueprintStepIndex: &stepIdx,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = 'running', session_id = ?, worktree_path = ? WHERE id = ?`, sessionID, worktreePath, runID); err != nil {
		t.Fatalf("update run: %v", err)
	}
}

// seedJiraRun is the Jira variant of seedRun. The task's entity is
// jira-sourced so Takeover's task-source gate (added with the lazy
// delegation rewrite) sees a Jira run rather than a GitHub PR run.
func seedJiraRun(t *testing.T, database *sql.DB, runID, sessionID, worktreePath string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-"+runID, "issue", "T-"+runID, "https://x/"+runID)
	if err != nil {
		t.Fatalf("create jira entity: %v", err)
	}
	eventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, runID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedRunBlueprint(t, database, runID, task.ID)
	stepIdx := 0
	if err := sqlitestore.New(database).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrg, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "test-prompt",
		Status: "running", Model: "claude-sonnet-4-6", WorktreePath: worktreePath,
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = 'running', session_id = ?, worktree_path = ? WHERE id = ?`, sessionID, worktreePath, runID); err != nil {
		t.Fatalf("update run: %v", err)
	}
}

// newSpawnerWithActiveCancel returns a spawner with one fake "active"
// run registered in the cancels map — Takeover's atomic active-check
// requires this to pass before doing any other work.
func newSpawnerWithActiveCancel(database *sql.DB, runID string) *Spawner {
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")
	if runID != "" {
		_, cancel := context.WithCancel(context.Background())
		s.cancels[runID] = cancel
	}
	return s
}

// TestTakeover_EmptyBaseDir is the cheapest validation: no DB or
// goroutine state needed. Returned error is a plain message, not a
// sentinel — empty base dir is a server config bug, not a client
// problem, and the handler routes uncategorized errors to 500.
func TestTakeover_EmptyBaseDir(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	_, err := s.Takeover(runmode.LocalDefaultOrg, "any-run", "", "test-user")
	if err == nil {
		t.Fatal("expected error on empty baseDir")
	}
	if errors.Is(err, ErrTakeoverInvalidState) ||
		errors.Is(err, ErrTakeoverInProgress) ||
		errors.Is(err, ErrTakeoverRaceLost) {
		t.Errorf("empty baseDir should not match a takeover sentinel; got %v", err)
	}
}

// TestTakeover_NonexistentRun: row doesn't exist → ErrTakeoverInvalidState.
// Maps to 400 in the handler.
func TestTakeover_NonexistentRun(t *testing.T) {
	database := newTakeoverTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "", "")

	_, err := s.Takeover(runmode.LocalDefaultOrg, "no-such-run", "/tmp/dest", "test-user")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_NoSessionID: row exists but session_id is empty (the
// agent hasn't produced its system/init event yet). Refuse — the
// resume command would have nothing to attach to.
func TestTakeover_NoSessionID(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-no-sid", "", "/tmp/wt")
	s := newSpawnerWithActiveCancel(database, "run-no-sid")

	_, err := s.Takeover(runmode.LocalDefaultOrg, "run-no-sid", "/tmp/dest", "test-user")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_NoWorktreePath: defensive — a run that for some reason
// has no worktree_path on its row (setup error, schema oddity) can't
// be taken over. Jira lazy runs DO populate worktree_path with the
// run-root (so a resume can reuse it as the resume cwd); the
// Jira-specific rejection happens at the next gate (task-source
// check, see TestTakeover_RejectsJiraLazyRun).
func TestTakeover_NoWorktreePath(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-no-wt", "sess-1", "")
	s := newSpawnerWithActiveCancel(database, "run-no-wt")

	_, err := s.Takeover(runmode.LocalDefaultOrg, "run-no-wt", "/tmp/dest", "test-user")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_RejectsJiraLazyRun: Jira lazy delegations populate
// runs.worktree_path (with the run-root) so a resume works, but
// they're not yet supported for takeover — multi-worktree relocation
// requires `git worktree move` per materialized worktree (SKY-234).
// Until that lands, refuse explicitly via the task-source check.
func TestTakeover_RejectsJiraLazyRun(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedJiraRun(t, database, "run-jira", "sess-1", "/tmp/runs/run-jira")
	s := newSpawnerWithActiveCancel(database, "run-jira")

	_, err := s.Takeover(runmode.LocalDefaultOrg, "run-jira", "/tmp/dest", "test-user")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
	if err == nil || !strings.Contains(err.Error(), "Jira lazy delegation") {
		t.Errorf("err = %v, expected message identifying Jira lazy delegation", err)
	}
}

// TestTakeover_NoActiveRun: the row passes validation, has a session,
// has a worktree path — but the cancels map doesn't have an entry,
// meaning the goroutine has already exited (run finished naturally
// just before we ran). Refuse; the handler maps this to 400.
func TestTakeover_NoActiveRun(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-not-active", "sess-1", "/tmp/wt")
	// No cancels[runID] set.
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "", "")

	_, err := s.Takeover(runmode.LocalDefaultOrg, "run-not-active", "/tmp/dest", "test-user")
	if !errors.Is(err, ErrTakeoverInvalidState) {
		t.Errorf("err = %v, want ErrTakeoverInvalidState", err)
	}
}

// TestTakeover_AlreadyInProgress: a second concurrent takeover for the
// same run hits the takenOver map check and gets ErrTakeoverInProgress.
// Maps to 409 in the handler. We pre-set the takenOver flag rather
// than running two real Takeover calls because the second one would
// race the first's later steps.
func TestTakeover_AlreadyInProgress(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-double", "sess-1", "/tmp/wt")
	s := newSpawnerWithActiveCancel(database, "run-double")
	s.takenOver["run-double"] = true

	_, err := s.Takeover(runmode.LocalDefaultOrg, "run-double", "/tmp/dest", "test-user")
	if !errors.Is(err, ErrTakeoverInProgress) {
		t.Errorf("err = %v, want ErrTakeoverInProgress", err)
	}
}

// TestWasTakenOver verifies the small accessor used by every gated
// cleanup path. A nil-safe read — the map is always initialized in
// NewSpawner — but cheap to assert.
func TestWasTakenOver(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	if s.wasTakenOver("missing") {
		t.Error("expected false for missing entry")
	}
	s.takenOver["present"] = true
	if !s.wasTakenOver("present") {
		t.Error("expected true after set")
	}
}

// TestAbortTakeover_KeepsFlagSticky is the regression guard for a
// subtle race: clearing s.takenOver[runID] in abortTakeover would let
// the runAgent goroutine's late-firing gates (handleCancelled, failRun,
// the deferred RemoveClaudeProjectDir, the natural-completion block)
// proceed with their normal cleanup the moment they re-read
// wasTakenOver. The goroutine's unconditional db.CompleteAgentRun
// would overwrite our 'takeover_failed' stop_reason with 'cancelled',
// and its RemoveClaudeProjectDir would run alongside ours.
//
// Leaving the flag set keeps every gate closed and abortTakeover the
// sole writer. This test pins that invariant down: after abortTakeover
// runs, wasTakenOver(runID) must still return true.
func TestAbortTakeover_KeepsFlagSticky(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-abort", "sess-x", "/tmp/wt-abort")
	s := newSpawnerWithActiveCancel(database, "run-abort")
	s.takenOver["run-abort"] = true

	s.abortTakeover(runmode.LocalDefaultOrg, "run-abort", "", "", runmode.LocalDefaultUserID)

	if !s.wasTakenOver("run-abort") {
		t.Error("abortTakeover cleared the takenOver flag — late goroutine gates can now race the rollback")
	}
}

// TestAbortTakeover_MarksRowTerminal: when the row is still in a
// non-terminal status (the typical post-failure state because the
// goroutine's gated handleCancelled didn't write anything), the
// rollback marks it cancelled with stop_reason='takeover_failed' so
// the UI sees a closed run instead of a phantom 'running'.
func TestAbortTakeover_MarksRowTerminal(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-mark", "sess-y", "/tmp/wt-mark")
	s := newSpawnerWithActiveCancel(database, "run-mark")
	s.takenOver["run-mark"] = true

	s.abortTakeover(runmode.LocalDefaultOrg, "run-mark", "", "", runmode.LocalDefaultUserID)

	got, err := sqlitestore.New(database).AgentRuns.Get(t.Context(), runmode.LocalDefaultOrg, "run-mark")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if got.Status != "cancelled" {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}
	if got.StopReason != "takeover_failed" {
		t.Errorf("StopReason = %q, want takeover_failed", got.StopReason)
	}
}

// TestAbortTakeover_PrunesStaleWorktreeRegistration is the regression
// guard for the destPath cleanup bug: abortTakeover used to call
// os.RemoveAll on the takeover destination, which removes the directory
// but leaves the bare's worktrees/<runID>/ metadata entry pointing at
// a now-missing path. Future `git worktree add` or `move` against the
// same runID would then fail with a "stale worktree" error until the
// next manual prune.
//
// Switching abortTakeover to worktree.RemoveAt fixes this — RemoveAt
// runs `git worktree prune` across all bares after the rmdir.
func TestAbortTakeover_PrunesStaleWorktreeRegistration(t *testing.T) {
	root := t.TempDir()
	// Use HOME to scope the worktree package's reposDir / pruneAll
	// sweep to a directory we control.
	t.Setenv("HOME", root)

	bareDir := filepath.Join(root, ".triagefactory", "repos", "owner", "repo.git")
	if err := os.MkdirAll(filepath.Dir(bareDir), 0755); err != nil {
		t.Fatalf("mkdir reposDir parent: %v", err)
	}
	gitArgs := []string{"-c", "init.defaultBranch=main"}
	if out, err := exec.Command("git", append(gitArgs, "init", "--bare", "-b", "feature", bareDir)...).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v\n%s", err, out)
	}

	// Seed a commit on "feature" via a scratch worktree.
	seed := filepath.Join(root, "seed")
	mustGit(t, root, "clone", bareDir, seed)
	mustGit(t, seed, "config", "user.email", "t@e.com")
	mustGit(t, seed, "config", "user.name", "T")
	mustGit(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, seed, "add", "README.md")
	mustGit(t, seed, "commit", "-m", "seed")
	mustGit(t, seed, "push", "origin", "feature")

	// Pretend a takeover got partway through: a linked worktree exists
	// at destPath, registered with the bare. abortTakeover must
	// dismantle this cleanly.
	destPath := filepath.Join(root, "takeovers", "run-orphaned")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		t.Fatalf("mkdir takeovers parent: %v", err)
	}
	mustGit(t, bareDir, "worktree", "add", "--no-checkout", destPath, "feature")

	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-orphaned", "sess-x", "/tmp/source-not-relevant-here")
	s := newSpawnerWithActiveCancel(database, "run-orphaned")
	s.takenOver["run-orphaned"] = true

	s.abortTakeover(runmode.LocalDefaultOrg, "run-orphaned", "", destPath, runmode.LocalDefaultUserID)

	// destPath dir is gone (was removed by RemoveAt's RemoveAll).
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Errorf("destPath should have been removed (err=%v)", err)
	}

	// And — the load-bearing assertion — the bare's worktree list no
	// longer references run-orphaned. Without the prune step in
	// RemoveAt, this would still show the dangling registration.
	listOut, err := exec.Command("git", "-C", bareDir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	list := string(listOut)
	if strings.Contains(list, "run-orphaned") {
		t.Errorf("bare still has stale worktree registration for run-orphaned after abortTakeover; output:\n%s", list)
	}
}

// mustGit runs a git command in the given dir, fatal-erroring with the
// combined output included so test failures are diagnosable. Mirrors
// the gitCmd helper in the worktree package's test file.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), os.Environ()...),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// TestAbortTakeover_PreservesTerminalRow: when the row already reached
// a terminal status (race-loss path: goroutine wrote a real outcome
// before our flag could land), the rollback must NOT overwrite it
// with 'cancelled' — the agent's actual result has to survive.
func TestAbortTakeover_PreservesTerminalRow(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "run-already-done", "sess-z", "/tmp/wt-done")
	// Simulate the goroutine winning the race: row is already
	// 'completed' before we run abortTakeover.
	if _, err := database.Exec(`UPDATE runs SET status = 'completed', stop_reason = 'end_turn' WHERE id = ?`, "run-already-done"); err != nil {
		t.Fatalf("force completed status: %v", err)
	}
	s := newSpawnerWithActiveCancel(database, "run-already-done")
	s.takenOver["run-already-done"] = true

	s.abortTakeover(runmode.LocalDefaultOrg, "run-already-done", "", "", runmode.LocalDefaultUserID)

	got, err := sqlitestore.New(database).AgentRuns.Get(t.Context(), runmode.LocalDefaultOrg, "run-already-done")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if got.Status != "completed" {
		t.Errorf("Status changed from completed to %q — abortTakeover must preserve the agent's real outcome", got.Status)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("StopReason changed from end_turn to %q", got.StopReason)
	}
}

// TestCanonicalizeForSafetyCheck_ExistingPath: returns the abs+clean
// form (deliberately NOT symlink-resolved — see the helper's comment
// for why). The path doesn't have to exist on disk for the function
// to succeed.
func TestCanonicalizeForSafetyCheck_ExistingPath(t *testing.T) {
	dir := t.TempDir()
	got, err := canonicalizeForSafetyCheck(dir)
	if err != nil {
		t.Fatalf("canonicalizeForSafetyCheck: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	expected := filepath.Clean(abs)
	if got != expected {
		t.Errorf("got %q, want filepath.Clean(filepath.Abs(...)) = %q", got, expected)
	}
}

// TestCanonicalizeForSafetyCheck_FallsBackForMissingPath is the
// load-bearing case for the manual-delete fix: when the takeover dir
// is gone, EvalSymlinks fails, but Release still needs a canonical
// form to compare against the takeover base. The fallback uses
// filepath.Abs(filepath.Clean(...)), which doesn't require the path
// to exist. Without this, a manually-deleted takeover dir wedged
// Release on its up-front EvalSymlinks call and stranded the run as
// permanently held.
func TestCanonicalizeForSafetyCheck_FallsBackForMissingPath(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "definitely-not-here")

	got, err := canonicalizeForSafetyCheck(missing)
	if err != nil {
		t.Fatalf("canonicalizeForSafetyCheck on missing path: %v", err)
	}
	expectedAbs, _ := filepath.Abs(missing)
	if got != filepath.Clean(expectedAbs) {
		t.Errorf("got %q, want filepath.Clean(filepath.Abs(...)) = %q", got, filepath.Clean(expectedAbs))
	}
}

// TestCanonicalizeForSafetyCheck_PrefixCheckSurvivesMissingPath proves
// the failure mode the fallback fixes: a missing takeover dir under
// an existing base must still pass the rel-based prefix check Release
// uses. Same canonicalization on both sides means the comparison stays
// consistent regardless of which side fell back.
func TestCanonicalizeForSafetyCheck_PrefixCheckSurvivesMissingPath(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "run-deleted-by-user")

	canonBase, err := canonicalizeForSafetyCheck(base)
	if err != nil {
		t.Fatalf("canonicalize base: %v", err)
	}
	canonPath, err := canonicalizeForSafetyCheck(missing)
	if err != nil {
		t.Fatalf("canonicalize missing path: %v", err)
	}
	rel, err := filepath.Rel(canonBase, canonPath)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Errorf("missing path %q wrongly judged outside base %q (rel=%q)", canonPath, canonBase, rel)
	}
}
