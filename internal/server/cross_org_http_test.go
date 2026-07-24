package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// End-to-end HTTP cross-org tests. Each test seeds two users (alice in
// orgA, bob in orgB) and a resource in orgA, signs both users in via
// the OAuth callback rig, and asserts:
//   - alice's session-scoped request → 200 (her own org)
//   - bob's session-scoped request → 404 (cross-org appears absent)
//
// 404 (not 403) is the deliberate signal: disclosing "this exists but
// you can't see it" would leak the row's existence across the tenancy
// boundary. Mirrors auth_handlers_test.go's bullet-5 test
// (TestAuthFlow_OrgMiddleware_CrossOrg404AndMember200) but exercises
// the session-orgID path that the handler sweep + WithTx wrap rely
// on, rather than the URL-segment org_id path.
//
// What these tests together prove:
//   - withSession populates ctxKeyOrgID from the session's
//     active_org_id, not from any URL parameter the caller could spoof
//   - handlers extract orgID via requireOrg and pass it into
//     tx.X.Get inside s.tx.WithTx so RLS sees the right claims
//   - the per-store RLS USING filter (see _Postgres_CrossOrgRLSDenied
//     suite in internal/db/postgres) returns no row for the cross-org
//     pair, which the handler surfaces as 404
//
// Background-service handlers (those that route through s.spawner.X,
// which the rig stubs as nil) are NOT covered here — they short-circuit
// before reaching the store layer when spawner is missing. Covering
// them requires wiring a spawner stub into the rig, which is a heavier
// lift than the read paths need.

func setupTwoOrgSession(t *testing.T, r *authRig) (alice, bob uuid.UUID, orgA uuid.UUID, sidA, sidB string) {
	t.Helper()
	alice = r.seedUser()
	orgA, _ = r.seedOrg(alice, "alice-org")
	bob = r.seedUser()
	// orgB is created and bob is its owner; the active org on bob's
	// session is set from the membership at driveCallback time. We
	// don't reference orgB by ID later — tests address orgA's resource
	// via URL and prove bob's session-scoped read can't see it.
	_, _ = r.seedOrg(bob, "bob-org")

	respA, _ := r.driveCallback(alice)
	sidA = r.sidFromResp(respA)
	respB, _ := r.driveCallback(bob)
	sidB = r.sidFromResp(respB)
	return
}

func TestCrossOrgHTTP_TaskGet(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	taskA := seedTaskInOrg(t, r, orgA, alice, "task-get")

	if got := r.requestWithSid("GET", "/api/tasks/"+taskA, sidA).StatusCode; got != http.StatusOK {
		t.Errorf("alice GET /api/tasks/%s = %d, want 200", taskA, got)
	}
	if got := r.requestWithSid("GET", "/api/tasks/"+taskA, sidB).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob GET /api/tasks/%s = %d, want 404 (cross-org leak)", taskA, got)
	}
}

func TestCrossOrgHTTP_ProjectGet(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	projectA := seedProjectInOrg(t, r, orgA, alice, "proj-get")

	if got := r.requestWithSid("GET", "/api/projects/"+projectA, sidA).StatusCode; got != http.StatusOK {
		t.Errorf("alice GET /api/projects/%s = %d, want 200", projectA, got)
	}
	if got := r.requestWithSid("GET", "/api/projects/"+projectA, sidB).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob GET /api/projects/%s = %d, want 404 (cross-org leak)", projectA, got)
	}
}

func TestCrossOrgHTTP_ConversationGet(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	runA := seedRunInOrg(t, r, orgA, alice, "run-get")

	if got := r.requestWithSid("GET", "/api/agent/conversations/"+runA, sidA).StatusCode; got != http.StatusOK {
		t.Errorf("alice GET /api/agent/conversations/%s = %d, want 200", runA, got)
	}
	if got := r.requestWithSid("GET", "/api/agent/conversations/"+runA, sidB).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob GET /api/agent/conversations/%s = %d, want 404 (cross-org leak)", runA, got)
	}
}

// TestCrossOrgHTTP_AgentArtifacts covers the run-scoped artifact read
// (TFAC-465): alice sees her run's artifacts endpoint (200), bob sees it as
// absent (404). The run read is the RLS-scoped authorization gate, so a
// non-member never learns the run exists, let alone reads its artifacts.
func TestCrossOrgHTTP_AgentArtifacts(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	runA := seedRunInOrg(t, r, orgA, alice, "run-arts")

	if got := r.requestWithSid("GET", "/api/agent/conversations/"+runA+"/artifacts", sidA).StatusCode; got != http.StatusOK {
		t.Errorf("alice GET /api/agent/conversations/%s/artifacts = %d, want 200", runA, got)
	}
	if got := r.requestWithSid("GET", "/api/agent/conversations/"+runA+"/artifacts", sidB).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob GET /api/agent/conversations/%s/artifacts = %d, want 404 (cross-org leak)", runA, got)
	}
}

// TestCrossOrgHTTP_TaskSwipe covers the mutating path: bob's swipe
// gesture against alice's task should appear as "task not found" to
// bob, not as a 200 with a state change applied, and not as a 500. The
// handler does a tx.Tasks.Get inside WithTx before any side effect; RLS
// returns nil for the cross-org pair, handler 404s, no state mutated.
func TestCrossOrgHTTP_TaskSwipe(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	taskA := seedTaskInOrg(t, r, orgA, alice, "task-swipe")

	body := `{"action":"claim","hesitation_ms":0}`
	if got := postWithSid(t, r, "/api/tasks/"+taskA+"/swipe", sidA, body); got != http.StatusOK {
		t.Errorf("alice POST swipe on own task = %d, want 200", got)
	}
	if got := postWithSid(t, r, "/api/tasks/"+taskA+"/swipe", sidB, body); got != http.StatusNotFound {
		t.Errorf("bob POST swipe on cross-org task = %d, want 404", got)
	}
}

// seedTaskInOrg inserts a fresh entity + event + task chain in the
// given org via admin (BYPASSRLS). Returns the task UUID.
func seedTaskInOrg(t *testing.T, r *authRig, orgID, userID uuid.UUID, suffix string) string {
	t.Helper()
	entityID := uuid.NewString()
	eventID := uuid.NewString()
	taskID := uuid.NewString()
	sourceID := suffix + "-" + entityID[:8]

	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'cross-org seed', '', '{}'::jsonb, now())
	`, entityID, orgID, sourceID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:ci_check_failed', '', $5, 'queued', 'pending', 0.5)
	`, taskID, orgID, userID, entityID, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// seedProjectInOrg inserts a project owned by the given user/team via
// admin pool. Projects need creator_user_id + team_id; the visibility
// is 'team' so the team membership RLS policy admits the owner.
func seedProjectInOrg(t *testing.T, r *authRig, orgID, userID uuid.UUID, name string) string {
	t.Helper()
	projectID := uuid.NewString()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, description, pinned_repos)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        $4, '', '[]'::jsonb)
	`, projectID, orgID, userID, name); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return projectID
}

// seedRunInOrg inserts a full entity → event → task → prompt → run
// chain. Run is a manual trigger with the seeded user as creator and a
// 'running' status so handleAgentStatus has live data to project.
func seedRunInOrg(t *testing.T, r *authRig, orgID, userID uuid.UUID, suffix string) string {
	t.Helper()
	taskID := seedTaskInOrg(t, r, orgID, userID, suffix)
	// prompts.id is text (not uuid); pass the slug directly. runs.prompt_id
	// FKs into prompts(id, org_id) so the ID stored here is what the run
	// references below.
	promptID := "p-" + suffix + "-" + uuid.NewString()[:8]
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        $4, '')
	`, promptID, orgID, userID, promptID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	blueprintRunID := seedBlueprintRunInOrg(t, r, orgID, userID, taskID)
	runID := uuid.NewString()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, task_id, team_id, prompt_id, status, model, creator_user_id, trigger_type, blueprint_run_id)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        $4, 'running', 'm', $5, 'manual', $6)
	`, runID, orgID, taskID, promptID, userID, blueprintRunID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// seedBlueprintRunInOrg mints a blueprint + manual blueprint_run for the
// given task in the given org via the admin pool, returning the
// blueprint_run id. runs.blueprint_run_id is NOT NULL and FKs
// blueprint_runs(id, org_id), so every multi-tenant run fixture needs a
// parent blueprint_run first. Manual trigger_type requires
// creator_user_id NOT NULL (blueprint_runs_creator_matches_trigger_type).
// The blueprint and blueprint_run inherit the task's team (first team in
// the org), matching how seedTaskInOrg / seedRunInOrg resolve team_id.
func seedBlueprintRunInOrg(t *testing.T, r *authRig, orgID, userID uuid.UUID, taskID string) string {
	t.Helper()
	blueprintID := uuid.NewString()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'cross-org seed blueprint', 'user')
	`, blueprintID, orgID, userID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	blueprintRunID := uuid.NewString()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt-test', '[]')
	`, blueprintRunID, orgID, userID, blueprintID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return blueprintRunID
}

// postWithSid fires a POST with the sid cookie + same-origin Origin
// header (so withCSRFOriginCheck doesn't reject it) + a JSON body.
// Returns the status code. Wraps requestWithSid by extending it with
// the body — the existing rig helper is GET-only.
func postWithSid(t *testing.T, r *authRig, path, sid, jsonBody string) int {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(jsonBody)))
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Code
}
