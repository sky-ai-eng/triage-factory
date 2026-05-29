package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"

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
// Pre-SKY-259 this helper lived in task_rules_handler_test.go; after the
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
	// SKY-261 B+: swipe-delegate and factory_delegate both call
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
	// SKY-261 C work: handlers now re-check team_agents.enabled before
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
	// Pass the production default port so handler tests that read
	// settings GET see a realistic Server.Port (3000) — keeps test
	// behavior aligned with production and lets future assertions
	// on server_port pass through to the schema-mirroring path.
	return New(database, stores, "", 3000)
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
// pin repos pass the validatePinnedRepos existence check (which now reads
// team_github_repos), and upserts the matching repo_profiles row the
// Curator's repo-materialization eventually wants more of (clone_url,
// default_branch). Post-SKY-375 the team's tracked set is the source of
// truth; the team_github_repos insert is accumulative so multiple seed
// calls don't clobber each other the way ReplaceForTeam would.
func seedConfiguredRepo(t *testing.T, s *Server, owner, repo string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO team_github_repos (team_id, owner, repo)
		VALUES (?, ?, ?)
		ON CONFLICT(team_id, owner, repo) DO NOTHING
	`, runmode.LocalDefaultTeamID, owner, repo); err != nil {
		t.Fatalf("track repo %s/%s on default team: %v", owner, repo, err)
	}
	if err := sqlitestore.New(s.db).Repos.Upsert(context.Background(), runmode.LocalDefaultOrgID, domain.RepoProfile{
		ID:            owner + "/" + repo,
		Owner:         owner,
		Repo:          repo,
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("seed configured repo %s/%s: %v", owner, repo, err)
	}
}
