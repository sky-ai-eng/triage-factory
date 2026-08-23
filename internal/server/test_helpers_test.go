package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newTestServer spins up an in-memory SQLite with the full schema +
// events catalog seed, registers all HTTP routes, and returns the Server.
// Each test gets its own DB so there's no cross-contamination.
//
// This helper used to live in task_rules_handler_test.go; after the
// unification it sits in a dedicated test_helpers_test.go so any
// handler-level test can use it without depending on a specific feature's
// test file.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	// swipe-delegate and factory_delegate both call
	// Agents.GetForOrg to stamp claim. Without an agents row, those
	// paths return 500 with "no agent bootstrapped." Seed the local
	// sentinel agent row so handler tests reach the actual logic
	// under test rather than short-circuiting on agent bootstrap.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	// Handlers now re-check team_agents.enabled before
	// stamping the bot claim (the spec's bot-disabled-team handling).
	// Production seeds this via BootstrapTeamAgent; tests need the
	// same row or every delegate gesture 409s.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO team_agents (team_id, agent_id, enabled) VALUES (?, ?, 1)`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed local team_agents: %v", err)
	}
	stores := sqlitestore.New(database)
	s := New(database, stores)
	// The team knowledge base, rooted in a per-test temp dir: the real
	// local-mode backend rather than a stub, so the KB routes are exercised
	// against the same plain-files store a local install runs on.
	s.SetTeamKB(kbstore.NewLocalAt(t.TempDir()))
	return s
}

// configureEventSources binds the test org's GitHub and Jira credentials in a
// mock keychain, making both sources AVAILABLE for the event-source gate the
// event-handler create routes run.
//
// A fixture calls it when the thing under test is authoring a handler rather
// than the credential state itself: production reaches every authoring surface
// behind a setup gate that already requires GitHub, so an org that can author
// a github rule always has one bound. It is not folded into newTestServer
// because just as many fixtures are about the UNCONFIGURED org, and a server
// that quietly arrived configured would take that state away from them.
func configureEventSources(t *testing.T, s *Server) {
	t.Helper()
	keyring.MockInit()
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: "https://github.com",
		GitHubPAT: "ghp-fixture",
		JiraURL:   "https://jira.example.com",
		JiraPAT:   "jira-fixture",
	}); err != nil {
		t.Fatalf("configure event sources: %v", err)
	}
}

// unconfigureEventSources resets the mock keychain, leaving the test org with
// no credential bound. Local secrets live in ONE process-wide mock bag, so a
// fixture that is about the unconfigured org has to say so: a sibling test's
// configureEventSources otherwise leaves its credentials behind and the "fresh
// org" is quietly a configured one.
func unconfigureEventSources(t *testing.T) {
	t.Helper()
	keyring.MockInit()
}

// fixtureUUID derives a stable uuid from a readable seed, so fixtures keep
// their legible names ("r_msg", "t_ba") while producing ids the handlers'
// path guards accept — those guards answer 404 for anything that isn't a uuid,
// because on Postgres the columns behind them are uuid typed. Same seed →
// same id, within a run and across runs, so a test can name an id it expects
// to see in a response without threading the value through.
func fixtureUUID(seed string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

// The settings resources, addressed the way the routes are: the org's under
// its org, the team's under its team. Tests run in local mode, so the org id is
// the sentinel tenant and the team segment takes the "default" alias.
func orgSettingsPath() string { return "/api/orgs/" + runmode.LocalDefaultOrgID + "/settings" }

func teamSettingsPath(teamID string) string { return "/api/teams/" + teamID + "/settings" }

func teamJiraProjectsPath(teamID string) string { return "/api/teams/" + teamID + "/jira-projects" }

// orgSettingsVersion reads the settings row's current concurrency token.
func orgSettingsVersion(t *testing.T, s *Server) int {
	t.Helper()
	rec := doJSON(t, s, "GET", orgSettingsPath(), nil)
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var out struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.Version
}

// patchOrgSettings sends an org-settings PATCH with the row's LIVE version
// folded in, which is what a client that just read the form would do. A test
// asserting the conflict behaviour itself passes the version explicitly instead
// (see the concurrency case), so this helper never hides the token from a test
// that is about it.
func patchOrgSettings(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	withVersion := map[string]any{"version": orgSettingsVersion(t, s)}
	for k, v := range body {
		withVersion[k] = v
	}
	return doJSON(t, s, "PATCH", orgSettingsPath(), withVersion)
}

// patchOrgSettingsOK is patchOrgSettings for the happy path: it fails the test
// on any non-200 and hands back the decoded settings resource, which is what
// the PATCH answers with (including the next version and any advisory
// `warning`).
func patchOrgSettingsOK(t *testing.T, s *Server, body map[string]any) map[string]any {
	t.Helper()
	rec := patchOrgSettings(t, s, body)
	if rec.Code != 200 {
		t.Fatalf("PATCH %s: %d: %s", orgSettingsPath(), rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode org settings response: %v", err)
	}
	return resp
}

// doJSON performs a JSON request against the server's mux and returns
// the response. Body may be nil.
func doJSON(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// seedConfiguredRepo tracks owner/repo on the default team so tests that
// pin repos pass the validatePinnedRepos existence check (which reads
// team_github_repos), and upserts the matching repositories row with
// the fields repo-materialization eventually wants more of (clone_url,
// default_branch). The team's tracked set is the source of truth; the
// team_github_repos insert is accumulative so multiple seed calls don't
// clobber each other the way ReplaceForTeam would.
//
// The registry row comes first: tracking references it by id.
// Returns the registry row id, which is how every repo route and every
// repo-identifying payload field addresses it.
func seedConfiguredRepo(t *testing.T, s *Server, owner, repo string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := sqlitestore.New(s.db).Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Repository{
		Owner:         owner,
		Repo:          repo,
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("seed configured repo %s/%s: %v", owner, repo, err)
	}
	// Scoped the same way the store's own resolver is, on all four columns of
	// the folded identity index. The fixture is single-org today, so org_id and
	// source cannot yet select the wrong row — which is the reason to bind them
	// now, while "there is only one" is an accident of the fixture rather than
	// something this query is entitled to assume.
	var repositoryID string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id FROM repositories
		 WHERE org_id = ? AND source = ?
		   AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
	`, runmode.LocalDefaultOrgID, domain.RepoSourceGitHub, owner, repo).Scan(&repositoryID); err != nil {
		t.Fatalf("resolve repository id for %s/%s: %v", owner, repo, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO team_github_repos (team_id, repository_id, org_id)
		VALUES (?, ?, ?)
		ON CONFLICT(team_id, repository_id) DO NOTHING
	`, runmode.LocalDefaultTeamID, repositoryID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("track repo %s/%s on default team: %v", owner, repo, err)
	}
	return repositoryID
}

// seedUntrackedRepo mints a registry row no team tracks — the registry is a
// superset of the tracked set, so this is a real state, and it is the one that
// separates "no such repository" from "not yours to pin".
func seedUntrackedRepo(t *testing.T, s *Server, owner, repo string) string {
	t.Helper()
	if _, err := sqlitestore.New(s.db).Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.Repository{
		Owner:         owner,
		Repo:          repo,
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("seed untracked repo %s/%s: %v", owner, repo, err)
	}
	return repoIDFor(t, s, owner, repo)
}

// repoIDFor resolves an already-seeded repository's registry id. Same folded
// lookup seedConfiguredRepo does, for the tests that seed through another
// path (or that need the id again after a rename moved the name).
func repoIDFor(t *testing.T, s *Server, owner, repo string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRowContext(context.Background(), `
		SELECT id FROM repositories
		 WHERE org_id = ? AND source = ?
		   AND LOWER(owner) = LOWER(?) AND LOWER(repo) = LOWER(?)
	`, runmode.LocalDefaultOrgID, domain.RepoSourceGitHub, owner, repo).Scan(&id); err != nil {
		t.Fatalf("resolve repository id for %s/%s: %v", owner, repo, err)
	}
	return id
}

// blueprintRunSeq makes the blueprint / blueprint_run IDs minted by
// seedBlueprintRunSQLite unique across calls within a single test
// process, so a fixture invoked more than once doesn't collide on the
// blueprints / blueprint_runs primary keys.
var blueprintRunSeq int

// seedBlueprintRunSQLite mints a blueprint + blueprint_run for the given
// task on the local-default org/team and returns the blueprint_run id.
// conversations.blueprint_run_id is NOT NULL, so every fixture that inserts
// a row into conversations must first create a blueprint_run for that task
// and point the run at it. SQLite's blueprint_runs has no
// org_id/creator_user_id, but
// worktree_path is NOT NULL.
func seedBlueprintRunSQLite(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	blueprintRunSeq++
	blueprintID := "bp_" + taskID
	blueprintRunID := "br_" + taskID
	if blueprintRunSeq > 1 {
		suffix := strconv.Itoa(blueprintRunSeq)
		blueprintID += "_" + suffix
		blueprintRunID += "_" + suffix
	}
	if _, err := database.Exec(
		`INSERT INTO blueprints (id, name, source, org_id, team_id, creator_user_id)
		 VALUES (?, 'Test Blueprint', 'user', ?, ?, ?)`,
		blueprintID, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		 VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt-test', '[]')`,
		blueprintRunID, blueprintID, taskID,
	); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return blueprintRunID
}
