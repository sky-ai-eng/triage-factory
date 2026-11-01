package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
)

// End-to-end HTTP cross-org tests. Each seeds alice in orgA, bob in orgB and a
// resource in orgA, signs both in, and asserts alice's request answers 200 and
// bob's 404: the org comes from the session's active_org_id, never the URL, and
// a 404 rather than a 403 keeps the row's existence inside its tenant.
//
// Handlers that go through s.spawner, which the rig leaves nil, are not
// covered here.

func setupTwoOrgSession(t *testing.T, r *authRig) (alice, bob uuid.UUID, orgA uuid.UUID, sidA, sidB string) {
	t.Helper()
	alice = r.seedUser()
	orgA, _ = r.seedOrg(alice, "alice-org")
	bob = r.seedUser()
	// bob owns orgB, which his session's active org is set to at sign-in.
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

func TestCrossOrgHTTP_ConversationGet(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	runA := seedConversationInOrg(t, r, orgA, alice, "run-get")

	if got := r.requestWithSid("GET", "/api/agent/conversations/"+runA, sidA).StatusCode; got != http.StatusOK {
		t.Errorf("alice GET /api/agent/conversations/%s = %d, want 200", runA, got)
	}
	if got := r.requestWithSid("GET", "/api/agent/conversations/"+runA, sidB).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob GET /api/agent/conversations/%s = %d, want 404 (cross-org leak)", runA, got)
	}
}

// TestCrossOrgHTTP_AgentDeliverables: the run-scoped deliverable list is gated
// by the RLS-scoped run read, so bob never learns alice's run exists.
func TestCrossOrgHTTP_AgentDeliverables(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	runA := seedConversationInOrg(t, r, orgA, alice, "run-arts")

	path := "/api/agent/conversations/" + runA + "/deliverables/list"
	if got := r.postJSONWithSid("POST", path, sidA, map[string]any{}).StatusCode; got != http.StatusOK {
		t.Errorf("alice POST %s = %d, want 200", path, got)
	}
	if got := r.postJSONWithSid("POST", path, sidB, map[string]any{}).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob POST %s = %d, want 404 (cross-org leak)", path, got)
	}
}

// TestCrossOrgHTTP_AgentActions: the run-scoped action list has no entitlement
// gate, so the run-visibility check is its whole authorization; a regression
// there would expose another org's audit log.
func TestCrossOrgHTTP_AgentActions(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	runA := seedConversationInOrg(t, r, orgA, alice, "run-acts")

	path := "/api/agent/conversations/" + runA + "/actions/list"
	if got := r.postJSONWithSid("POST", path, sidA, map[string]any{}).StatusCode; got != http.StatusOK {
		t.Errorf("alice POST %s = %d, want 200", path, got)
	}
	if got := r.postJSONWithSid("POST", path, sidB, map[string]any{}).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob POST %s = %d, want 404 (cross-org leak)", path, got)
	}
}

// TestCrossOrgHTTP_AgentArtifacts is the same check for the run-scoped
// artifact read, which reads the same ledger as the action list and is gated
// the same way: by the run read alone.
func TestCrossOrgHTTP_AgentArtifacts(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	runA := seedConversationInOrg(t, r, orgA, alice, "run-artifacts")

	// One agent row on the run's pull request, so alice's read returns a row
	// through the app pool's policies: the grouped ledger read and its
	// entities join, under her claims.
	var entityID, sourceID string
	if err := r.h.AdminDB.QueryRow(`
		SELECT e.id::text, e.source_id FROM conversations c
		  JOIN tasks t ON t.id = c.task_id JOIN entities e ON e.id = t.entity_id
		 WHERE c.id = $1`, runA).Scan(&entityID, &sourceID); err != nil {
		t.Fatalf("read the run's entity: %v", err)
	}
	const rowURL = "https://github.com/octo/pull/1"
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO external_actions (org_id, team_id, provider, scope, action, target, url, conversation_id,
		                              credential, dedup_key, entity_id, performed_by, object_kind)
		VALUES ($1, (SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1),
		        'github', 'https://github.com', 'pr_created', 'stale/key#1', $2, $3,
		        'github_app', gen_random_uuid()::text, $4, 'agent', 'pull_request')
	`, orgA, rowURL, runA, entityID); err != nil {
		t.Fatalf("seed ledger row: %v", err)
	}

	path := "/api/agent/conversations/" + runA + "/artifacts/list"
	resp := r.postJSONWithSid("POST", path, sidA, map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alice POST %s = %d, want 200", path, resp.StatusCode)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode artifacts: %v", err)
	}
	_ = resp.Body.Close()
	// The seeded entity has an empty url, so the row's stands in.
	if len(page.Items) != 1 || page.Items[0]["target"] != sourceID || page.Items[0]["url"] != rowURL {
		t.Errorf("alice's artifacts = %v, want one with target %q and url %q", page.Items, sourceID, rowURL)
	}
	if got := r.postJSONWithSid("POST", path, sidB, map[string]any{}).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob POST %s = %d, want 404 (cross-org leak)", path, got)
	}

	conv := r.requestWithSid("GET", "/api/agent/conversations/"+runA, sidA)
	var m map[string]any
	if err := json.NewDecoder(conv.Body).Decode(&m); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	_ = conv.Body.Close()
	if m["artifact_count"] != float64(1) {
		t.Errorf("alice's artifact_count = %v, want 1", m["artifact_count"])
	}
}

// TestCrossOrgHTTP_TaskClaim: bob's claim on alice's task is a 404, not a 200
// with the claim applied or a 500, because the handler reads the task under RLS
// before any side effect.
func TestCrossOrgHTTP_TaskClaim(t *testing.T) {
	r := newAuthRig(t)
	alice, _, orgA, sidA, sidB := setupTwoOrgSession(t, r)
	taskA := seedTaskInOrg(t, r, orgA, alice, "task-claim")

	body := map[string]any{"hesitation_ms": 0}
	if got := r.postJSONWithSid("POST", "/api/tasks/"+taskA+"/claim", sidA, body).StatusCode; got != http.StatusOK {
		t.Errorf("alice POST claim on own task = %d, want 200", got)
	}
	if got := r.postJSONWithSid("POST", "/api/tasks/"+taskA+"/claim", sidB, body).StatusCode; got != http.StatusNotFound {
		t.Errorf("bob POST claim on cross-org task = %d, want 404", got)
	}
}

// seedTaskInOrg inserts a fresh entity + event + task chain in the
// given org via admin (BYPASSRLS). Returns the task UUID.
func seedTaskInOrg(t *testing.T, r *authRig, orgID, userID uuid.UUID, suffix string) string {
	t.Helper()
	entityID := uuid.NewString()
	eventID := uuid.NewString()
	taskID := uuid.NewString()
	sourceID := "octo/" + suffix + "#" + entityID[:8]
	repoID := dbseed.GitHubEntityRepository(t, r.h.AdminDB, orgID.String(), "https://github.com", sourceID)

	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, scope, repository_id)
		VALUES ($1, $2, 'github', $3, 'pr', 'cross-org seed', '', '{}'::jsonb, now(), 'https://github.com', $4)
	`, entityID, orgID, sourceID, repoID); err != nil {
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

// seedConversationInOrg inserts a full entity → event → task → prompt → run
// chain. Run is a manual trigger with the seeded user as creator and a
// 'running' status so handleAgentStatus has live data to project.
func seedConversationInOrg(t *testing.T, r *authRig, orgID, userID uuid.UUID, suffix string) string {
	t.Helper()
	taskID := seedTaskInOrg(t, r, orgID, userID, suffix)
	// prompts.id is text (not uuid); pass the slug directly. conversations.prompt_id
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
	conversationID := uuid.NewString()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, task_id, team_id, prompt_id, status, model, creator_user_id, trigger_type, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        $4, 'running', 'm', $5, 'manual', $6, 0)
	`, conversationID, orgID, taskID, promptID, userID, blueprintRunID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return conversationID
}

// seedBlueprintRunInOrg mints a blueprint and a manual blueprint_run for taskID
// on the org's first team and returns the run id. conversations.blueprint_run_id
// is NOT NULL, so every conversation fixture needs one.
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
