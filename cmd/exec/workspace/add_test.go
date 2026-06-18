package workspace

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	_ "modernc.org/sqlite"
)

// hostFor wraps a Stores + runID into a LocalClient with the routing
// identity tests want. Used by the materializeWorkspace cases below
// where the test seeds a run row and then drives materialization
// against its id. Equivalent to what agenthost.AutoDetect produces in
// local mode.
func hostFor(stores db.Stores, runID string) agenthost.Client {
	return agenthost.NewLocal(stores, agenthost.RunInfo{
		OrgID:  runmode.LocalDefaultOrgID,
		UserID: runmode.LocalDefaultUserID,
		RunID:  runID,
	})
}

func TestSplitOwnerRepo(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"sky-ai-eng/triage-factory", "sky-ai-eng", "triage-factory", true},
		{"a/b", "a", "b", true},

		// Malformed inputs all reject — no half-parsed owner/repo.
		{"", "", "", false},
		{"no-slash", "", "", false},
		{"/missing-owner", "", "", false},
		{"missing-repo/", "", "", false},
		{"too/many/slashes", "", "", false},
		{"trailing/", "", "", false},
		{"a/b/", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			owner, repo, ok := splitOwnerRepo(c.in)
			if owner != c.wantOwner || repo != c.wantRepo || ok != c.wantOK {
				t.Errorf("splitOwnerRepo(%q) = (%q, %q, %v); want (%q, %q, %v)",
					c.in, owner, repo, ok, c.wantOwner, c.wantRepo, c.wantOK)
			}
		})
	}
}

// newTestDB spins up an in-memory SQLite with the full schema so the
// orchestration tests run against the real DB layer (FK cascades,
// INSERT OR IGNORE on the run_worktrees PK, the actual queries).
// Mocking DB calls would test less of the actual code under change.
//
// Returns the assembled db.Stores bundle (the post-SKY-302 dependency
// shape that runAdd/materializeWorkspace consume) plus the bare
// *db.DB for tests that still seed via the legacy sqlitestore.New
// helpers.
func newTestDB(t *testing.T) (db.Stores, *db.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return sqlitestore.New(conn), &db.DB{Conn: conn}
}

// seedBlueprintRun mints a fresh blueprint + blueprint_run for taskID
// and returns its id. runs.blueprint_run_id is NOT NULL, so every
// seeded run needs a parent blueprint_run. SQLite blueprint_runs has no
// org_id/creator_user_id columns; org_id on blueprints takes its
// local-sentinel DEFAULT.
func seedBlueprintRun(t *testing.T, conn *sql.DB, taskID string) string {
	t.Helper()
	bpID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, name, source, team_id, creator_user_id)
		VALUES (?, 'Test BP', 'user', ?, ?)
	`, bpID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt', '[]')
	`, brID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

func seedJiraRun(t *testing.T, database *db.DB, runID, issueKey string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database.Conn).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", issueKey, "issue", "T-"+issueKey, "https://x/"+issueKey)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := sqlitestore.New(database.Conn).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := sqlitestore.New(database.Conn).Tasks.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, runID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := sqlitestore.New(database.Conn).Prompts.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := sqlitestore.New(database.Conn).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
		BlueprintRunID: seedBlueprintRun(t, database.Conn, task.ID),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func seedGitHubRun(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database.Conn).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#"+runID, "pr", "T", "https://x/"+runID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := sqlitestore.New(database.Conn).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := sqlitestore.New(database.Conn).Tasks.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, runID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := sqlitestore.New(database.Conn).Prompts.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := sqlitestore.New(database.Conn).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
		BlueprintRunID: seedBlueprintRun(t, database.Conn, task.ID),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func seedRepoProfile(t *testing.T, database *db.DB, owner, repo, cloneURL, defaultBranch string) {
	t.Helper()
	if err := sqlitestore.New(database.Conn).Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: owner + "/" + repo, Owner: owner, Repo: repo,
		CloneURL: cloneURL, DefaultBranch: defaultBranch,
		ProfileText: "test profile",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
}

// expectedPath returns the deterministic worktree path materializeWorkspace
// will compute for a given runID + owner/repo. Computed via the same
// worktree.RunRoot helper so test assertions stay aligned with the runtime.
func expectedPath(runID, owner, repo string) string {
	return filepath.Join(worktree.RunRoot(runID), owner, repo)
}

// stubCalls records create/remove/stat invocations and returns canned
// responses. Defaults are tuned for "happy first add against an empty
// run":
//   - createPath="" → create stub returns the deterministic production
//     path so most tests don't need to set it.
//   - statPath defaults to ErrNotExist (no path is "live" until a test
//     puts something in liveDirs).
//   - now defaults to time.Now (real clock); tests that exercise the
//     stale-reservation gate override fixedNow.
type stubCalls struct {
	mu sync.Mutex

	createCalls int
	createArgs  []createCall
	createPath  string
	createErr   error

	removeCalls int
	removePaths []string

	// liveDirs is the set of paths the stat stub treats as existing
	// directories (returns a non-nil FileInfo, no error). Anything
	// else returns ErrNotExist.
	liveDirs map[string]struct{}

	// fixedNow, if non-zero, is what `now()` returns. Used to drive the
	// stale-reservation age gate without sleeping in tests.
	fixedNow time.Time
}

type createCall struct {
	owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string
}

// fakeFileInfo is the minimal os.FileInfo the stat stub returns for a
// "live" path. Only IsDir() is consulted by the orchestration today,
// but the rest of the surface stays present for forward compatibility.
type fakeFileInfo struct{ name string }

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return true }
func (f fakeFileInfo) Sys() any           { return nil }

func (s *stubCalls) deps() addDeps {
	return addDeps{
		createWorktree: func(_ context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.createCalls++
			s.createArgs = append(s.createArgs, createCall{owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot})
			if s.createErr != nil {
				return "", s.createErr
			}
			path := s.createPath
			if path == "" {
				// Default to the deterministic path the production
				// implementation would produce, so tests don't need
				// to set createPath unless they need a specific value.
				path = filepath.Join(runRoot, owner, repo)
			}
			return path, nil
		},
		removeWorktree: func(path, _ string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.removeCalls++
			s.removePaths = append(s.removePaths, path)
			return nil
		},
		statPath: func(path string) (os.FileInfo, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, ok := s.liveDirs[path]; ok {
				return fakeFileInfo{name: filepath.Base(path)}, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
		},
		now: func() time.Time {
			if !s.fixedNow.IsZero() {
				return s.fixedNow
			}
			return time.Now()
		},
	}
}

func TestMaterializeWorkspace_MissingRunID(t *testing.T) {
	stores, _ := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(hostFor(stores, ""), "owner/repo", stub.deps())
	if !errors.Is(err, errMissingRunID) {
		t.Errorf("err = %v, want errMissingRunID", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called %d times on missing run id; should not be invoked before validation", stub.createCalls)
	}
}

func TestMaterializeWorkspace_InvalidOwnerRepo(t *testing.T) {
	stores, _ := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(hostFor(stores, "r1"), "no-slash", stub.deps())
	if !errors.Is(err, errInvalidOwnerRepo) {
		t.Errorf("err = %v, want errInvalidOwnerRepo", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called %d times on invalid owner/repo", stub.createCalls)
	}
}

func TestMaterializeWorkspace_RunNotFound(t *testing.T) {
	stores, _ := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(hostFor(stores, "missing-run"), "owner/repo", stub.deps())
	if !errors.Is(err, errRunNotFound) {
		t.Errorf("err = %v, want errRunNotFound", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for missing run")
	}
}

func TestMaterializeWorkspace_RejectsGitHubPRRun(t *testing.T) {
	stores, database := newTestDB(t)
	seedGitHubRun(t, database, "gh-run")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "gh-run"), "owner/repo", stub.deps())
	if !errors.Is(err, errNotJiraRun) {
		t.Errorf("err = %v, want errNotJiraRun", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for GitHub PR run; should be rejected before create")
	}
}

func TestValidateEntityKey(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"SKY-220", true},
		{"sky-220", true},
		{"PROJ/sub.task_1", true},
		{"a", true},

		{"", false},
		{"-foo", false}, // leading dash → could be git flag
		{"--upload-pack=evil", false},
		{"foo;bar", false},
		{"foo bar", false},
		{"foo..bar", false}, // path-traversal / illegal refname
		{"foo\nbar", false},
		{"$(whoami)", false},
		{"foo:bar", false},
		{strings.Repeat("a", 200), false}, // length cap
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			err := validateEntityKey(c.in)
			if c.wantOK && err != nil {
				t.Errorf("validateEntityKey(%q) = %v, want nil", c.in, err)
			}
			if !c.wantOK && err == nil {
				t.Errorf("validateEntityKey(%q) = nil, want error", c.in)
			}
		})
	}
}

func TestMaterializeWorkspace_RejectsInjectionEntityKey(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "--upload-pack=evil")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if !errors.Is(err, errInvalidEntityKey) {
		t.Errorf("err = %v, want errInvalidEntityKey", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called with injection-shaped key")
	}
}

func TestMaterializeWorkspace_RepoNotConfigured(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "owner/repo", stub.deps())
	if !errors.Is(err, errRepoNotConfigured) {
		t.Errorf("err = %v, want errRepoNotConfigured", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for unconfigured repo")
	}
}

func TestMaterializeWorkspace_RepoMissingCloneURL(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "owner", "repo", "" /*cloneURL*/, "main")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "owner/repo", stub.deps())
	if !errors.Is(err, errRepoMissingCloneURL) {
		t.Errorf("err = %v, want errRepoMissingCloneURL", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for profile with empty clone URL")
	}
}

func TestMaterializeWorkspace_SuccessfulFirstAdd(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-220")
	seedRepoProfile(t, database, "sky", "core", "https://github.com/sky/core.git", "main")
	stub := &stubCalls{}

	wantPath := expectedPath("r1", "sky", "core")
	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if stub.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", stub.createCalls)
	}
	args := stub.createArgs[0]
	if args.owner != "sky" || args.repo != "core" {
		t.Errorf("create args owner/repo = %s/%s, want sky/core", args.owner, args.repo)
	}
	if args.cloneURL != "https://github.com/sky/core.git" {
		t.Errorf("cloneURL = %q", args.cloneURL)
	}
	if args.featureBranch != "feature/SKY-220" {
		t.Errorf("featureBranch = %q, want feature/SKY-220", args.featureBranch)
	}
	if args.baseBranch != "main" {
		t.Errorf("baseBranch = %q, want main", args.baseBranch)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeWorktree called %d times on success path", stub.removeCalls)
	}

	// Verify the row landed with the deterministic path.
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepo(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core")
	if err != nil {
		t.Fatalf("GetRunWorktreeByRepo: %v", err)
	}
	if row == nil {
		t.Fatal("expected run_worktrees row, got nil")
	}
	if row.Path != wantPath {
		t.Errorf("row.Path = %q, want %q", row.Path, wantPath)
	}
	if row.FeatureBranch != "feature/SKY-220" {
		t.Errorf("row.FeatureBranch = %q", row.FeatureBranch)
	}
}

func TestMaterializeWorkspace_BaseBranchFallsBackToDefault(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	// BaseBranch empty → use DefaultBranch.
	seedRepoProfile(t, database, "owner", "repo", "https://x", "develop")
	stub := &stubCalls{}

	if _, err := materializeWorkspace(hostFor(stores, "r1"), "owner/repo", stub.deps()); err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if stub.createArgs[0].baseBranch != "develop" {
		t.Errorf("baseBranch = %q, want develop", stub.createArgs[0].baseBranch)
	}
}

func TestMaterializeWorkspace_IdempotentSecondAdd(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	stub := &stubCalls{}

	if _, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps()); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if stub.createCalls != 1 {
		t.Fatalf("first add createCalls = %d, want 1", stub.createCalls)
	}

	// Second add: GetRunWorktreeByRepo returns the row, so we
	// short-circuit before reservation/create.
	path2, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if path2 != wantPath {
		t.Errorf("idempotent path = %q, want %q", path2, wantPath)
	}
	if stub.createCalls != 1 {
		t.Errorf("createWorktree called %d times across two adds; second add should short-circuit on the precheck", stub.createCalls)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeWorktree called on idempotent re-add")
	}
}

func TestMaterializeWorkspace_RaceLossAtReservation(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	// Simulate a concurrent add that won the reservation race: insert
	// the winning row directly, with a distinguishable path so we can
	// confirm the loser returns IT and not its own pre-computed path.
	winnerPath := "/tmp/somewhere-else/winner"
	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: winnerPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed winner row: %v", err)
	}

	stub := &stubCalls{}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != winnerPath {
		t.Errorf("path = %q, want %q (winner's path)", path, winnerPath)
	}
	if stub.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0; loser must NOT touch git", stub.createCalls)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeCalls = %d, want 0; loser has nothing to remove", stub.removeCalls)
	}
}

func TestMaterializeWorkspace_TrustsReservationEvenWhenDirMissing(t *testing.T) {
	// Regression test for the in-flight-winner race the prior
	// stat-based stale-row branch reintroduced: when a concurrent
	// `workspace add` has reserved the row but its createWorktree is
	// still in flight, the on-disk path doesn't exist yet. The loser
	// must NOT delete the row and re-reserve — that would let both
	// processes proceed to create the same target dir, defeating the
	// PK-based serialization. Instead, return the winner's path; the
	// agent's subsequent `cd` succeeds once the winner's create
	// completes (or fails loudly if the winner errors out, in which
	// case the winner releases the reservation and a retry succeeds).
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	winnerPath := expectedPath("r1", "sky", "core")
	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: winnerPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed winner row: %v", err)
	}
	// The on-disk dir at winnerPath does NOT exist (we never created
	// it; production stat would return ErrNotExist). Production code
	// must still trust the row.
	stub := &stubCalls{}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != winnerPath {
		t.Errorf("path = %q, want %q (winner's path returned even though dir missing)", path, winnerPath)
	}
	if stub.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0; loser must not create when a reservation already exists", stub.createCalls)
	}
	// And the row must still be present — the loser must not have
	// deleted it.
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepo(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core")
	if err != nil {
		t.Fatalf("GetRunWorktreeByRepo: %v", err)
	}
	if row == nil {
		t.Fatal("winner's reservation row was deleted by the loser; expected it to remain")
	}
}

func TestMaterializeWorkspace_LiveDirShortCircuitsAgeCheck(t *testing.T) {
	// When the reserved path exists on disk, the row is honored
	// regardless of age — we don't need the time-based gate to defend
	// the in-flight-winner case once the create is observably done.
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: wantPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	stub := &stubCalls{
		liveDirs: map[string]struct{}{wantPath: {}},
		// Force `now` far enough ahead that the row would be "stale" by
		// the threshold — but it shouldn't matter because the path
		// exists and we short-circuit before the age check.
		fixedNow: time.Now().Add(staleReservationAge + time.Hour),
	}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if stub.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0; live row should short-circuit", stub.createCalls)
	}
}

func TestMaterializeWorkspace_StaleReservationReclaimed(t *testing.T) {
	// killed-mid-create scenario: a previous `workspace add` won the
	// row but its CreateForBranchInRoot got killed before completing,
	// so the path doesn't exist on disk and the row is older than the
	// staleReservationAge threshold. A subsequent retry must drop the
	// row and re-reserve — without this, the agent's `cd` fails forever.
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	// Seed the stale row (path won't exist on disk; default stub
	// statPath returns ErrNotExist for everything not in liveDirs).
	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: wantPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	stub := &stubCalls{
		fixedNow: time.Now().Add(staleReservationAge + time.Minute),
	}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1; stale reservation should not block recreate", stub.createCalls)
	}
	// And a fresh row exists post-reclaim.
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepo(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core")
	if err != nil || row == nil {
		t.Fatalf("expected fresh row after reclaim; got row=%v err=%v", row, err)
	}
}

func TestMaterializeWorkspace_FreshRowMissingDirIsInFlight(t *testing.T) {
	// Mirror of the staleReservation test, but with the row JUST
	// inserted (within the threshold). The orchestration should NOT
	// reclaim — the row is presumed to belong to an in-flight winner.
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: wantPath, FeatureBranch: "feature/SKY-1",
	}); err != nil {
		t.Fatalf("seed in-flight row: %v", err)
	}

	// Force `now` to be well within the threshold (real time may have
	// drifted since the seed; pin to a value tied to the row's
	// created_at via a re-read).
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepo(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core")
	if err != nil || row == nil {
		t.Fatalf("re-read row: %v", err)
	}
	stub := &stubCalls{
		fixedNow: row.CreatedAt.Add(staleReservationAge / 2),
	}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q (fresh row honored even with missing dir)", path, wantPath)
	}
	if stub.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0; fresh row must not be reclaimed", stub.createCalls)
	}
}

func TestMaterializeWorkspace_CreateFailureReleasesReservation(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	// Make createWorktree fail (e.g. network error fetching the bare).
	stub := &stubCalls{createErr: errors.New("simulated git failure")}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub.deps())
	if err == nil {
		t.Fatal("expected error from materializeWorkspace, got nil")
	}
	if !strings.Contains(err.Error(), "simulated git failure") {
		t.Errorf("err = %v, expected to wrap 'simulated git failure'", err)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", stub.createCalls)
	}

	// The reservation must have been released so the next attempt
	// can re-reserve. Verify the row is gone.
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepo(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core")
	if err != nil {
		t.Fatalf("GetRunWorktreeByRepo: %v", err)
	}
	if row != nil {
		t.Errorf("expected run_worktrees row to be released after create failure, found %+v", row)
	}
}

func TestMaterializeWorkspace_CreateFailureRetryable(t *testing.T) {
	// End-to-end of the release-on-failure contract: a first attempt
	// fails (createWorktree errors), reservation is released, a second
	// attempt succeeds.
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core")

	stub1 := &stubCalls{createErr: errors.New("network blip")}
	if _, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub1.deps()); err == nil {
		t.Fatal("expected first-attempt failure")
	}

	stub2 := &stubCalls{}
	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", stub2.deps())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if path != wantPath {
		t.Errorf("retry path = %q, want %q", path, wantPath)
	}
	if stub2.createCalls != 1 {
		t.Errorf("retry createCalls = %d, want 1", stub2.createCalls)
	}
}

func TestMaterializeWorkspace_TooManySlashesRejected(t *testing.T) {
	// Inputs with extra slashes must reject at parse time rather than
	// synthesizing a repoID like "too/many/slashes" that no configured
	// repo could match — that would surface as a misleading "repo is
	// not configured" instead of "invalid owner/repo." The rest of the
	// codebase validates repo slugs as exactly one slash; this gate
	// matches that.
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "too/many/slashes", stub.deps())
	if !errors.Is(err, errInvalidOwnerRepo) {
		t.Errorf("err = %v, want errInvalidOwnerRepo", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createWorktree called for malformed input")
	}
}

// seedEventTriggeredJiraRun mirrors seedJiraRun but stamps the agent
// run with trigger_type='event' and a NULL creator_user_id — the
// shape the schema CHECK requires for auto-delegated runs. Used by
// the SKY-302 routing tests to exercise the admin-pool branch of
// insertRunWorktreeReservation / deleteRunWorktreeReservation
// against the same SQLite fixture as the manual-branch tests.
func seedEventTriggeredJiraRun(t *testing.T, database *db.DB, runID, issueKey string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database.Conn).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", issueKey, "issue", "T-"+issueKey, "https://x/"+issueKey)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := sqlitestore.New(database.Conn).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := sqlitestore.New(database.Conn).Tasks.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, runID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := sqlitestore.New(database.Conn).Prompts.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	// trigger_type='event' + CreatorUserID="" → schema CHECK
	// requires creator_user_id IS NULL; Create's SQLite impl maps
	// the empty string to SQL NULL when trigger_type='event'.
	if err := sqlitestore.New(database.Conn).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
		TriggerType:    "event",
		BlueprintRunID: seedBlueprintRun(t, database.Conn, task.ID),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestMaterializeWorkspace_EventTriggeredRunRoutingSKY302 verifies
// that an event-triggered run (trigger_type='event', no
// creator_user_id) successfully materializes a workspace through the
// admin-pool branch added in SKY-302. In SQLite the two branches
// collapse to identical SQL but the code path differs; this test
// pins the manual-only seedJiraRun's coverage from regressing the
// event-triggered shape, which would silently break under Postgres
// once SKY-303 lifts cmd/exec to the host daemon.
func TestMaterializeWorkspace_EventTriggeredRunRoutingSKY302(t *testing.T) {
	stores, database := newTestDB(t)
	seedEventTriggeredJiraRun(t, database, "e1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{}

	path, err := materializeWorkspace(hostFor(stores, "e1"), "sky/core", stub.deps())
	if err != nil {
		t.Fatalf("event-triggered materializeWorkspace: %v", err)
	}
	if path == "" {
		t.Error("expected non-empty path returned for event-triggered run")
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1; event-triggered path should reserve + create", stub.createCalls)
	}
	// Verify the reservation row landed.
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepo(context.Background(), runmode.LocalDefaultOrgID, "e1", "sky/core")
	if err != nil || row == nil {
		t.Fatalf("expected run_worktrees row from event-triggered insert; got row=%v err=%v", row, err)
	}
}

// TestMaterializeWorkspace_EventTriggeredCreateFailureReleasesSKY302
// is the event-triggered counterpart of
// TestMaterializeWorkspace_CreateFailureReleasesReservation. The
// release-on-failure path goes through the admin-pool DeleteByRepo
// variant when the run is event-triggered; without it a failed
// create would strand a reservation that the next retry can't
// clear.
func TestMaterializeWorkspace_EventTriggeredCreateFailureReleasesSKY302(t *testing.T) {
	stores, database := newTestDB(t)
	seedEventTriggeredJiraRun(t, database, "e2", "SKY-2")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{createErr: errors.New("simulated git failure")}

	if _, err := materializeWorkspace(hostFor(stores, "e2"), "sky/core", stub.deps()); err == nil {
		t.Fatal("expected error from create failure, got nil")
	}
	// Reservation must be released so a retry can re-reserve.
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepo(context.Background(), runmode.LocalDefaultOrgID, "e2", "sky/core")
	if err != nil {
		t.Fatalf("GetRunWorktreeByRepo: %v", err)
	}
	if row != nil {
		t.Errorf("expected event-triggered reservation to be released after create failure, found %+v", row)
	}
}
