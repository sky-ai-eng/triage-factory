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
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
	// Production seeds this via BootstrapLocalAgent; tests need the
	// same row or every delegate gesture 409s.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO team_agents (team_id, agent_id, enabled) VALUES (?, ?, 1)`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID,
	); err != nil {
		t.Fatalf("seed local team_agents: %v", err)
	}
	stores := sqlitestore.New(database)
	return New(database, stores)
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
// team_github_repos), and upserts the matching repositories row the
// Curator's repo-materialization eventually wants more of (clone_url,
// default_branch). The team's tracked set is the source of truth; the
// team_github_repos insert is accumulative so multiple seed calls don't
// clobber each other the way ReplaceForTeam would.
//
// The registry row comes first: tracking references it by id.
func seedConfiguredRepo(t *testing.T, s *Server, owner, repo string) {
	t.Helper()
	ctx := context.Background()
	if err := sqlitestore.New(s.db).Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Repository{
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
