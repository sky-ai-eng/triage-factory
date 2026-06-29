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
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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
		TeamID: runmode.LocalDefaultTeamID,
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

func TestParseAddArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantRepo    string
		wantSpec    checkoutSpec
		wantErr     error // errors.Is target; nil = expect success
		wantErrText string
	}{
		{name: "default checkout", args: []string{"sky/core"}, wantRepo: "sky/core", wantSpec: checkoutSpec{}},
		{name: "ref space form", args: []string{"sky/core", "--ref", "feature-x"}, wantRepo: "sky/core", wantSpec: checkoutSpec{ref: "feature-x"}},
		{name: "ref equals form", args: []string{"sky/core", "--ref=feature-x"}, wantRepo: "sky/core", wantSpec: checkoutSpec{ref: "feature-x"}},
		{name: "ref before positional", args: []string{"--ref", "feature-x", "sky/core"}, wantRepo: "sky/core", wantSpec: checkoutSpec{ref: "feature-x"}},
		{name: "pr space form", args: []string{"sky/core", "--pr", "42"}, wantRepo: "sky/core", wantSpec: checkoutSpec{pr: 42}},
		{name: "pr equals form", args: []string{"sky/core", "--pr=42"}, wantRepo: "sky/core", wantSpec: checkoutSpec{pr: 42}},

		{name: "missing owner/repo", args: []string{"--ref", "x"}, wantErr: errMissingOwnerRepo},
		{name: "too many positionals", args: []string{"a/b", "c/d"}, wantErr: errInvalidOwnerRepo},
		{name: "ref and pr conflict", args: []string{"sky/core", "--ref", "x", "--pr", "1"}, wantErr: errRefAndPR},
		{name: "pr non-numeric", args: []string{"sky/core", "--pr", "abc"}, wantErr: errInvalidPR},
		{name: "pr zero", args: []string{"sky/core", "--pr", "0"}, wantErr: errInvalidPR},
		{name: "pr negative", args: []string{"sky/core", "--pr", "-3"}, wantErr: errInvalidPR},
		{name: "unknown flag", args: []string{"sky/core", "--wat"}, wantErrText: "unknown flag"},
		{name: "ref without value", args: []string{"sky/core", "--ref"}, wantErrText: "--ref requires"},
		{name: "pr without value", args: []string{"sky/core", "--pr"}, wantErrText: "--pr requires"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, spec, err := parseAddArgs(c.args)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, c.wantErr)
				}
				return
			}
			if c.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrText) {
					t.Fatalf("err = %v, want text containing %q", err, c.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if repo != c.wantRepo || spec != c.wantSpec {
				t.Errorf("parseAddArgs(%v) = (%q, %+v); want (%q, %+v)", c.args, repo, spec, c.wantRepo, c.wantSpec)
			}
		})
	}
}

// newTestDB spins up an in-memory SQLite with the full schema so the
// orchestration tests run against the real DB layer (FK cascades,
// INSERT OR IGNORE on the run_worktrees PK, the actual queries).
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
// seeded run needs a parent blueprint_run.
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
	store := sqlitestore.New(database.Conn)
	if err := store.Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: owner + "/" + repo, Owner: owner, Repo: repo,
		CloneURL: cloneURL, DefaultBranch: defaultBranch,
		ProfileText: "test profile",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	// Track the repo for the run's team too — materializeWorkspace gates
	// `workspace add` on team-tracking (org-configured ≠ team-tracked), so an
	// org-only seed would be rejected. Additive so a test seeding several repos
	// keeps them all tracked.
	tracked, err := store.TeamGitHubRepos.ListForTeamSystem(context.Background(), runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("list team repos: %v", err)
	}
	tracked = append(tracked, domain.TeamGitHubRepo{Owner: owner, Repo: repo})
	if err := store.TeamGitHubRepos.ReplaceForTeam(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, tracked); err != nil {
		t.Fatalf("track team repo: %v", err)
	}
}

// expectedPath returns the deterministic worktree path materializeWorkspace
// will compute for a given runID + owner/repo + ref-slug.
func expectedPath(runID, owner, repo, ref string) string {
	return filepath.Join(worktree.RunRoot(runID), owner, repo, ref)
}

// stubCalls records checkout / PR-checkout / fetchPR / remove / stat
// invocations and returns canned responses. Defaults are tuned for "happy
// first add against an empty run":
//   - createPath="" → checkout/checkoutPR return the deterministic production
//     path so most tests don't need to set it.
//   - statPath defaults to ErrNotExist (no path is "live" until a test puts
//     something in liveDirs).
//   - now defaults to time.Now (real clock); tests that exercise the
//     stale-reservation gate override fixedNow.
type stubCalls struct {
	mu sync.Mutex

	checkoutCalls int
	checkoutArgs  []checkoutCall

	prCheckoutCalls int
	prCheckoutArgs  []prCheckoutCall

	// createPath / createErr drive BOTH checkout and checkoutPR returns.
	createPath string
	createErr  error

	fetchPRCalls int
	prView       *ghclient.PRView
	prErr        error

	removeCalls int
	removePaths []string

	liveDirs map[string]struct{}
	fixedNow time.Time
}

type checkoutCall struct {
	owner, repo, cloneURL, ref, runID, runRoot string
}

type prCheckoutCall struct {
	owner, repo, upstream, head, headBranch, runID, runRoot string
	prNumber                                                int
}

// fakeFileInfo is the minimal os.FileInfo the stat stub returns for a
// "live" path. Only IsDir() is consulted by the orchestration today.
type fakeFileInfo struct{ name string }

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return true }
func (f fakeFileInfo) Sys() any           { return nil }

func (s *stubCalls) deps() addDeps {
	// pathOrDefault mirrors the InRoot create contract: land at
	// {runRoot}/{owner}/{repo}/{ref-slug}. The slug is computed from the same
	// helpers production uses so the stub's returned path matches the path
	// materializeWorkspace reserved (no spurious divergence warning).
	pathOrDefault := func(runRoot, owner, repo, slug string) (string, error) {
		if s.createErr != nil {
			return "", s.createErr
		}
		if s.createPath != "" {
			return s.createPath, nil
		}
		return filepath.Join(runRoot, owner, repo, slug), nil
	}
	return addDeps{
		checkout: func(_ context.Context, owner, repo, cloneURL, ref, runID, runRoot string) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.checkoutCalls++
			s.checkoutArgs = append(s.checkoutArgs, checkoutCall{owner, repo, cloneURL, ref, runID, runRoot})
			return pathOrDefault(runRoot, owner, repo, worktree.CheckoutRefSlug(ref))
		},
		checkoutPR: func(_ context.Context, owner, repo, upstream, head, headBranch string, prNumber int, runID, runRoot string, _ ...worktree.CloneOption) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.prCheckoutCalls++
			s.prCheckoutArgs = append(s.prCheckoutArgs, prCheckoutCall{owner, repo, upstream, head, headBranch, runID, runRoot, prNumber})
			return pathOrDefault(runRoot, owner, repo, worktree.PRRefSlug(prNumber))
		},
		fetchPR: func(_ context.Context, _, _ string, _ int) (*ghclient.PRView, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.fetchPRCalls++
			return s.prView, s.prErr
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
	_, err := materializeWorkspace(hostFor(stores, ""), "owner/repo", checkoutSpec{}, stub.deps())
	if !errors.Is(err, errMissingRunID) {
		t.Errorf("err = %v, want errMissingRunID", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called %d times on missing run id; should not be invoked before validation", stub.checkoutCalls)
	}
}

func TestMaterializeWorkspace_InvalidOwnerRepo(t *testing.T) {
	stores, _ := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(hostFor(stores, "r1"), "no-slash", checkoutSpec{}, stub.deps())
	if !errors.Is(err, errInvalidOwnerRepo) {
		t.Errorf("err = %v, want errInvalidOwnerRepo", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called %d times on invalid owner/repo", stub.checkoutCalls)
	}
}

func TestMaterializeWorkspace_RunNotFound(t *testing.T) {
	stores, _ := newTestDB(t)
	stub := &stubCalls{}
	_, err := materializeWorkspace(hostFor(stores, "missing-run"), "owner/repo", checkoutSpec{}, stub.deps())
	if !errors.Is(err, errRunNotFound) {
		t.Errorf("err = %v, want errRunNotFound", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called for missing run")
	}
}

// TestMaterializeWorkspace_GitHubRunCanAdd pins the dropped Jira gate (TFAC-498):
// a GitHub run can now materialize a workspace, where it used to be rejected.
func TestMaterializeWorkspace_GitHubRunCanAdd(t *testing.T) {
	stores, database := newTestDB(t)
	seedGitHubRun(t, database, "gh-run")
	seedRepoProfile(t, database, "sky", "core", "https://github.com/sky/core.git", "main")
	stub := &stubCalls{}

	path, err := materializeWorkspace(hostFor(stores, "gh-run"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace on a GitHub run: %v", err)
	}
	if path != expectedPath("gh-run", "sky", "core", "@default") {
		t.Errorf("path = %q", path)
	}
	if stub.checkoutCalls != 1 {
		t.Errorf("checkoutCalls = %d, want 1", stub.checkoutCalls)
	}
}

func TestValidateGitRef(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"SKY-220", true},
		{"feature/foo", true},
		{"release/1.2.x", true},
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
		{"refs/heads/main", false}, // would double up into +refs/heads/refs/heads/main:...
		{"refs/tags/v1", false},
		{"feature/", false},               // trailing slash → git refuses the refname
		{strings.Repeat("a", 200), false}, // length cap
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			err := validateGitRef(c.in)
			if c.wantOK && err != nil {
				t.Errorf("validateGitRef(%q) = %v, want nil", c.in, err)
			}
			if !c.wantOK && err == nil {
				t.Errorf("validateGitRef(%q) = nil, want error", c.in)
			}
		})
	}
}

func TestMaterializeWorkspace_RejectsInjectionRef(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{ref: "--upload-pack=evil"}, stub.deps())
	if !errors.Is(err, errInvalidRef) {
		t.Errorf("err = %v, want errInvalidRef", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called with injection-shaped ref")
	}
}

func TestMaterializeWorkspace_RepoNotConfigured(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "owner/repo", checkoutSpec{}, stub.deps())
	if !errors.Is(err, errRepoNotConfigured) {
		t.Errorf("err = %v, want errRepoNotConfigured", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called for unconfigured repo")
	}
}

func TestMaterializeWorkspace_RepoMissingCloneURL(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "owner", "repo", "" /*cloneURL*/, "main")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "owner/repo", checkoutSpec{}, stub.deps())
	if !errors.Is(err, errRepoMissingCloneURL) {
		t.Errorf("err = %v, want errRepoMissingCloneURL", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called for profile with empty clone URL")
	}
}

func TestMaterializeWorkspace_DefaultBranchCheckout(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-220")
	seedRepoProfile(t, database, "sky", "core", "https://github.com/sky/core.git", "main")
	stub := &stubCalls{}

	wantPath := expectedPath("r1", "sky", "core", "@default")
	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if stub.checkoutCalls != 1 || stub.prCheckoutCalls != 0 {
		t.Fatalf("checkoutCalls=%d prCheckoutCalls=%d, want 1/0", stub.checkoutCalls, stub.prCheckoutCalls)
	}
	args := stub.checkoutArgs[0]
	if args.owner != "sky" || args.repo != "core" {
		t.Errorf("checkout owner/repo = %s/%s, want sky/core", args.owner, args.repo)
	}
	if args.cloneURL != "https://github.com/sky/core.git" {
		t.Errorf("cloneURL = %q", args.cloneURL)
	}
	if args.ref != "" {
		t.Errorf("ref = %q, want empty (default branch)", args.ref)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeWorktree called %d times on success path", stub.removeCalls)
	}

	// The row records the deterministic path and ref "@default" — a detached
	// default checkout claims no working branch (the push gate reads the
	// worktree's live branch, set once the agent makes its own); the ref is the
	// materialization selector, not a working branch.
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "@default")
	if err != nil || row == nil {
		t.Fatalf("GetByRepoRef: row=%v err=%v", row, err)
	}
	if row.Path != wantPath {
		t.Errorf("row.Path = %q, want %q", row.Path, wantPath)
	}
	if row.Ref != "@default" {
		t.Errorf("row.Ref = %q, want @default for a default checkout", row.Ref)
	}
}

func TestMaterializeWorkspace_RefCheckout(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{}

	if _, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{ref: "feature-x"}, stub.deps()); err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if stub.checkoutCalls != 1 {
		t.Fatalf("checkoutCalls = %d, want 1", stub.checkoutCalls)
	}
	// The git fetch ref is the RAW branch (feature-x); the stored run_worktrees
	// ref is the namespaced slug (ref-feature-x).
	if got := stub.checkoutArgs[0].ref; got != "feature-x" {
		t.Errorf("ref = %q, want feature-x", got)
	}
	row, _ := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "ref-feature-x")
	if row == nil || row.Ref != "ref-feature-x" {
		t.Errorf("row.Ref = %v, want ref-feature-x", row)
	}
}

func TestMaterializeWorkspace_PRCheckout(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://github.com/sky/core.git", "main")
	stub := &stubCalls{
		prView: &ghclient.PRView{
			HeadRef:  "contrib-branch",
			CloneURL: "https://github.com/fork/core.git", // fork (HTTPS) head
			SSHURL:   "git@github.com:fork/core.git",
		},
	}

	wantPath := expectedPath("r1", "sky", "core", "pr-42")
	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{pr: 42}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if stub.fetchPRCalls != 1 {
		t.Errorf("fetchPRCalls = %d, want 1", stub.fetchPRCalls)
	}
	if stub.prCheckoutCalls != 1 || stub.checkoutCalls != 0 {
		t.Fatalf("prCheckoutCalls=%d checkoutCalls=%d, want 1/0", stub.prCheckoutCalls, stub.checkoutCalls)
	}
	pc := stub.prCheckoutArgs[0]
	if pc.prNumber != 42 {
		t.Errorf("prNumber = %d, want 42", pc.prNumber)
	}
	if pc.headBranch != "contrib-branch" {
		t.Errorf("headBranch = %q, want contrib-branch", pc.headBranch)
	}
	// Upstream is the profile clone URL; head is the fork's URL in the matching
	// (HTTPS) protocol.
	if pc.upstream != "https://github.com/sky/core.git" {
		t.Errorf("upstream = %q", pc.upstream)
	}
	if pc.head != "https://github.com/fork/core.git" {
		t.Errorf("head = %q", pc.head)
	}
	row, _ := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "pr-42")
	if row == nil || row.Ref != "pr-42" {
		t.Errorf("row.Ref = %v, want pr-42", row)
	}
}

func TestMaterializeWorkspace_PRFetchFailureReleasesReservation(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://github.com/sky/core.git", "main")
	stub := &stubCalls{prErr: errors.New("github said no")}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{pr: 7}, stub.deps())
	if err == nil || !strings.Contains(err.Error(), "github said no") {
		t.Fatalf("err = %v, want it to wrap 'github said no'", err)
	}
	if stub.prCheckoutCalls != 0 {
		t.Errorf("prCheckoutCalls = %d, want 0 (fetch failed before create)", stub.prCheckoutCalls)
	}
	// Reservation released so a retry can re-reserve.
	row, _ := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "pr-7")
	if row != nil {
		t.Errorf("expected reservation released after PR-fetch failure, found %+v", row)
	}
}

// TestMaterializeWorkspace_PRFetchWiredFromHost leaves deps.fetchPR nil so the
// orchestration wires it to host.GithubGetPR. With no GitHub credentials the
// host returns an error, which must propagate (and release the reservation) —
// proving the host seam is the production PR-fetch path.
func TestMaterializeWorkspace_PRFetchWiredFromHost(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://github.com/sky/core.git", "main")
	stub := &stubCalls{}
	d := stub.deps()
	d.fetchPR = nil // force the host wiring

	if _, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{pr: 1}, d); err == nil {
		t.Fatal("expected an error from the host PR fetch (no GitHub credentials configured)")
	}
	row, _ := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "pr-1")
	if row != nil {
		t.Errorf("expected reservation released after host PR-fetch failure, found %+v", row)
	}
}

// TestMaterializeWorkspace_RejectsUntrackedRepo pins the team-tracking gate.
func TestMaterializeWorkspace_RejectsUntrackedRepo(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-9")
	// Org-configured but team-untracked: seed only the profile.
	if err := sqlitestore.New(database.Conn).Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID: "sky/untracked", Owner: "sky", Repo: "untracked",
		CloneURL: "https://x", DefaultBranch: "main", ProfileText: "test",
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	stub := &stubCalls{}

	if _, err := materializeWorkspace(hostFor(stores, "r1"), "sky/untracked", checkoutSpec{}, stub.deps()); !errors.Is(err, errRepoNotTracked) {
		t.Fatalf("err = %v, want errRepoNotTracked", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called for an untracked repo; want the gate to reject before create")
	}
}

func TestMaterializeWorkspace_IdempotentSecondAdd(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core", "@default")

	stub := &stubCalls{}

	if _, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps()); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if stub.checkoutCalls != 1 {
		t.Fatalf("first add checkoutCalls = %d, want 1", stub.checkoutCalls)
	}

	path2, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if path2 != wantPath {
		t.Errorf("idempotent path = %q, want %q", path2, wantPath)
	}
	if stub.checkoutCalls != 1 {
		t.Errorf("checkout called %d times across two adds; second add should short-circuit on the precheck", stub.checkoutCalls)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeWorktree called on idempotent re-add")
	}
}

func TestMaterializeWorkspace_RaceLossAtReservation(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	winnerPath := "/tmp/somewhere-else/winner"
	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: winnerPath, Ref: "@default",
	}); err != nil {
		t.Fatalf("seed winner row: %v", err)
	}

	stub := &stubCalls{}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != winnerPath {
		t.Errorf("path = %q, want %q (winner's path)", path, winnerPath)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkoutCalls = %d, want 0; loser must NOT touch git", stub.checkoutCalls)
	}
	if stub.removeCalls != 0 {
		t.Errorf("removeCalls = %d, want 0; loser has nothing to remove", stub.removeCalls)
	}
}

func TestMaterializeWorkspace_TrustsReservationEvenWhenDirMissing(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	winnerPath := expectedPath("r1", "sky", "core", "@default")
	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: winnerPath, Ref: "@default",
	}); err != nil {
		t.Fatalf("seed winner row: %v", err)
	}
	stub := &stubCalls{}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != winnerPath {
		t.Errorf("path = %q, want %q (winner's path returned even though dir missing)", path, winnerPath)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkoutCalls = %d, want 0; loser must not create when a reservation already exists", stub.checkoutCalls)
	}
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "@default")
	if err != nil {
		t.Fatalf("GetByRepoRef: %v", err)
	}
	if row == nil {
		t.Fatal("winner's reservation row was deleted by the loser; expected it to remain")
	}
}

func TestMaterializeWorkspace_LiveDirShortCircuitsAgeCheck(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core", "@default")

	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: wantPath, Ref: "@default",
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	stub := &stubCalls{
		liveDirs: map[string]struct{}{wantPath: {}},
		fixedNow: time.Now().Add(staleReservationAge + time.Hour),
	}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkoutCalls = %d, want 0; live row should short-circuit", stub.checkoutCalls)
	}
}

func TestMaterializeWorkspace_StaleReservationReclaimed(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core", "@default")

	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: wantPath, Ref: "@default",
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	stub := &stubCalls{
		fixedNow: time.Now().Add(staleReservationAge + time.Minute),
	}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if stub.checkoutCalls != 1 {
		t.Errorf("checkoutCalls = %d, want 1; stale reservation should not block recreate", stub.checkoutCalls)
	}
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "@default")
	if err != nil || row == nil {
		t.Fatalf("expected fresh row after reclaim; got row=%v err=%v", row, err)
	}
}

func TestMaterializeWorkspace_FreshRowMissingDirIsInFlight(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core", "@default")

	if _, _, err := sqlitestore.New(database.Conn).RunWorktrees.Insert(context.Background(), runmode.LocalDefaultOrgID, domain.RunWorktree{
		RunID: "r1", RepoID: "sky/core",
		Path: wantPath, Ref: "@default",
	}); err != nil {
		t.Fatalf("seed in-flight row: %v", err)
	}

	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "@default")
	if err != nil || row == nil {
		t.Fatalf("re-read row: %v", err)
	}
	stub := &stubCalls{
		fixedNow: row.CreatedAt.Add(staleReservationAge / 2),
	}

	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("materializeWorkspace: %v", err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q (fresh row honored even with missing dir)", path, wantPath)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkoutCalls = %d, want 0; fresh row must not be reclaimed", stub.checkoutCalls)
	}
}

func TestMaterializeWorkspace_CreateFailureReleasesReservation(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")

	stub := &stubCalls{createErr: errors.New("simulated git failure")}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub.deps())
	if err == nil {
		t.Fatal("expected error from materializeWorkspace, got nil")
	}
	if !strings.Contains(err.Error(), "simulated git failure") {
		t.Errorf("err = %v, expected to wrap 'simulated git failure'", err)
	}
	if stub.checkoutCalls != 1 {
		t.Errorf("checkoutCalls = %d, want 1", stub.checkoutCalls)
	}

	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "r1", "sky/core", "@default")
	if err != nil {
		t.Fatalf("GetByRepoRef: %v", err)
	}
	if row != nil {
		t.Errorf("expected run_worktrees row to be released after create failure, found %+v", row)
	}
}

func TestMaterializeWorkspace_CreateFailureRetryable(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	wantPath := expectedPath("r1", "sky", "core", "@default")

	stub1 := &stubCalls{createErr: errors.New("network blip")}
	if _, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub1.deps()); err == nil {
		t.Fatal("expected first-attempt failure")
	}

	stub2 := &stubCalls{}
	path, err := materializeWorkspace(hostFor(stores, "r1"), "sky/core", checkoutSpec{}, stub2.deps())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if path != wantPath {
		t.Errorf("retry path = %q, want %q", path, wantPath)
	}
	if stub2.checkoutCalls != 1 {
		t.Errorf("retry checkoutCalls = %d, want 1", stub2.checkoutCalls)
	}
}

func TestMaterializeWorkspace_TooManySlashesRejected(t *testing.T) {
	stores, database := newTestDB(t)
	seedJiraRun(t, database, "r1", "SKY-1")
	stub := &stubCalls{}

	_, err := materializeWorkspace(hostFor(stores, "r1"), "too/many/slashes", checkoutSpec{}, stub.deps())
	if !errors.Is(err, errInvalidOwnerRepo) {
		t.Errorf("err = %v, want errInvalidOwnerRepo", err)
	}
	if stub.checkoutCalls != 0 {
		t.Errorf("checkout called for malformed input")
	}
}

func TestPRCloneURLs(t *testing.T) {
	cases := []struct {
		name         string
		origin       string
		pr           *ghclient.PRView
		wantUpstream string
		wantHead     string
	}{
		{
			name:         "https own-repo",
			origin:       "https://github.com/sky/core.git",
			pr:           &ghclient.PRView{CloneURL: "https://github.com/sky/core.git", SSHURL: "git@github.com:sky/core.git"},
			wantUpstream: "https://github.com/sky/core.git",
			wantHead:     "https://github.com/sky/core.git",
		},
		{
			name:         "https fork",
			origin:       "https://github.com/sky/core.git",
			pr:           &ghclient.PRView{CloneURL: "https://github.com/fork/core.git", SSHURL: "git@github.com:fork/core.git"},
			wantUpstream: "https://github.com/sky/core.git",
			wantHead:     "https://github.com/fork/core.git",
		},
		{
			name:         "ssh fork uses ssh head form",
			origin:       "git@github.com:sky/core.git",
			pr:           &ghclient.PRView{CloneURL: "https://github.com/fork/core.git", SSHURL: "git@github.com:fork/core.git"},
			wantUpstream: "git@github.com:sky/core.git",
			wantHead:     "git@github.com:fork/core.git",
		},
		{
			name:         "deleted fork (no head urls) → empty head, read-only",
			origin:       "https://github.com/sky/core.git",
			pr:           &ghclient.PRView{CloneURL: "", SSHURL: ""},
			wantUpstream: "https://github.com/sky/core.git",
			wantHead:     "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, head := prCloneURLs(c.origin, c.pr)
			if up != c.wantUpstream || head != c.wantHead {
				t.Errorf("prCloneURLs(%q) = (%q, %q); want (%q, %q)", c.origin, up, head, c.wantUpstream, c.wantHead)
			}
		})
	}
}

func TestRefForSpec(t *testing.T) {
	cases := []struct {
		spec checkoutSpec
		want string
	}{
		{checkoutSpec{}, "@default"},
		{checkoutSpec{ref: "feature-x"}, "ref-feature-x"},
		{checkoutSpec{ref: "feature/foo"}, "ref-feature~foo"}, // '/' → '~'
		{checkoutSpec{pr: 42}, "pr-42"},
		// A branch literally named "pr-42" must NOT collide with PR #42's
		// reserved "pr-42" slug — the "ref-" namespace keeps them disjoint.
		{checkoutSpec{ref: "pr-42"}, "ref-pr-42"},
	}
	for _, c := range cases {
		if got := refForSpec(c.spec); got != c.want {
			t.Errorf("refForSpec(%+v) = %q, want %q", c.spec, got, c.want)
		}
	}

	// Injectivity guard: distinct refs that the old fold-to-'-' scheme collapsed
	// must now map to distinct slugs.
	if a, b := refForSpec(checkoutSpec{ref: "feature/foo"}), refForSpec(checkoutSpec{ref: "feature-foo"}); a == b {
		t.Errorf("refForSpec collision: feature/foo and feature-foo both → %q", a)
	}
	if a, b := refForSpec(checkoutSpec{ref: "pr-42"}), refForSpec(checkoutSpec{pr: 42}); a == b {
		t.Errorf("refForSpec collision: branch pr-42 and PR #42 both → %q", a)
	}
}

// seedEventTriggeredJiraRun mirrors seedJiraRun but stamps the agent
// run with trigger_type='event' and a NULL creator_user_id — the
// shape the schema CHECK requires for auto-delegated runs.
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
	if err := sqlitestore.New(database.Conn).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
		TriggerType:    "event",
		BlueprintRunID: seedBlueprintRun(t, database.Conn, task.ID),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestMaterializeWorkspace_EventTriggeredRunRouting verifies that an
// event-triggered run materializes through the admin-pool branch.
func TestMaterializeWorkspace_EventTriggeredRunRouting(t *testing.T) {
	stores, database := newTestDB(t)
	seedEventTriggeredJiraRun(t, database, "e1", "SKY-1")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{}

	path, err := materializeWorkspace(hostFor(stores, "e1"), "sky/core", checkoutSpec{}, stub.deps())
	if err != nil {
		t.Fatalf("event-triggered materializeWorkspace: %v", err)
	}
	if path == "" {
		t.Error("expected non-empty path returned for event-triggered run")
	}
	if stub.checkoutCalls != 1 {
		t.Errorf("checkoutCalls = %d, want 1; event-triggered path should reserve + create", stub.checkoutCalls)
	}
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "e1", "sky/core", "@default")
	if err != nil || row == nil {
		t.Fatalf("expected run_worktrees row from event-triggered insert; got row=%v err=%v", row, err)
	}
}

// TestMaterializeWorkspace_EventTriggeredCreateFailureReleases is the
// event-triggered counterpart of the release-on-failure path.
func TestMaterializeWorkspace_EventTriggeredCreateFailureReleases(t *testing.T) {
	stores, database := newTestDB(t)
	seedEventTriggeredJiraRun(t, database, "e2", "SKY-2")
	seedRepoProfile(t, database, "sky", "core", "https://x", "main")
	stub := &stubCalls{createErr: errors.New("simulated git failure")}

	if _, err := materializeWorkspace(hostFor(stores, "e2"), "sky/core", checkoutSpec{}, stub.deps()); err == nil {
		t.Fatal("expected error from create failure, got nil")
	}
	row, err := sqlitestore.New(database.Conn).RunWorktrees.GetByRepoRef(context.Background(), runmode.LocalDefaultOrgID, "e2", "sky/core", "@default")
	if err != nil {
		t.Fatalf("GetByRepoRef: %v", err)
	}
	if row != nil {
		t.Errorf("expected event-triggered reservation to be released after create failure, found %+v", row)
	}
}
