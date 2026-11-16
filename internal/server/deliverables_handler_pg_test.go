package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/deliverables"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestDeliverableReject_TeammateClosesTheTaskOfACompletedRun_Postgres: one
// member of a team delegates, the run completes with one draft pull request,
// and another member of the team rejects it. The rejection resolved the
// task's last deliverable, so the task closes as the run's, exactly as it
// would for the delegator. The close reads the run under the resolving
// member's claims, so it is only right if the run is readable by its team.
func TestDeliverableReject_TeammateClosesTheTaskOfACompletedRun_Postgres(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)
	orgID, delegator, teamID := pgtest.SeedOrgWithUser(t, h, "delegator")
	teammate := pgtest.SeedUser(t, h, "teammate")
	pgtest.AddOrgMember(t, h, teammate, orgID, teamID, "member", "member")

	// The org's GitHub is a stub, reached through an App installation on
	// acme, seeded on the admin pool as the App's install flow would.
	rs, stub := serveRejectStub(t, livePR("open", true, false), liveNode("OPEN", true, false), http.StatusNoContent, "")
	seed := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	seedAppIn(t, seed.Orgs, seed.Secrets, seed.GitHubApps, orgID, stub, acmeInstall())

	// The delegator's run: a bot-claimed task on the team, the completed run
	// and its concluded step on the same team, and the draft pull request the
	// step left awaiting a person.
	a := dbtest.InOrgScope(t, seed.Orgs, orgID, domain.NewPullRequestDeliverable("acme/api", 42, "PR_node", "feature/x", "main",
		"https://example.test/acme/api/pull/42", "Proposed title", "Proposed body", true))
	nodeID, _ := domain.DeliverablePullRequestNodeID(a)
	entity, _, err := seed.Entities.FindOrCreateSystem(ctx, orgID, "github", a.Scope, a.Target, nodeID, "pr", "Proposed title", a.URL)
	if err != nil {
		t.Fatalf("seed the pull request entity: %v", err)
	}
	agentID, eventID, taskID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	pgtest.MustExec(t, h.AdminDB, `INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Bot')`, agentID, orgID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
	`, eventID, orgID, entity.ID, domain.EventGitHubPRCICheckFailed)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key,
		                   primary_event_id, status, claimed_by_agent_id)
		VALUES ($1, $2, $3, $4, 'team', $5, $6, '', $7, 'in_progress', $8)
	`, taskID, orgID, delegator, teamID, entity.ID, domain.EventGitHubPRCICheckFailed, eventID, agentID)
	promptID, blueprintID, runID, conversationID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools)
		VALUES ($1, $2, $3, $4, 'P', 'body', 'user', '')
	`, promptID, orgID, delegator, teamID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source)
		VALUES ($1, $2, $3, $4, 'BP', 'user')
	`, blueprintID, orgID, teamID, delegator)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprint_runs (id, org_id, team_id, creator_user_id, blueprint_id, task_id, trigger_type,
		                            status, worktree_path, step_plan, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'manual', 'completed', '/tmp/wt', '[]', now())
	`, runID, orgID, teamID, delegator, blueprintID, taskID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO conversations (id, org_id, team_id, visibility, creator_user_id, task_id, prompt_id, trigger_type,
		                           status, completed_at, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, 'team', $4, $5, $6, 'manual', 'open', now(), $7, 0)
	`, conversationID, orgID, teamID, delegator, taskID, promptID, runID)
	a.OrgID, a.TeamID, a.ConversationID, a.EntityID = orgID, teamID, conversationID, entity.ID
	stored, err := seed.Deliverables.UpsertSystem(ctx, orgID, a)
	if err != nil {
		t.Fatalf("seed the draft pull request: %v", err)
	}

	rc := reconcile.NewReconciler(s.ghResolver, stores, reconcile.TrustRecords, nil)
	mux := http.NewServeMux()
	mount := func(pattern string, hf http.HandlerFunc) { mux.HandleFunc(pattern, hf) }
	deliverables.New(s.tx, s.az, s.ws, s.ghResolver, nil,
		func() *reconcile.Reconciler { return rc }, func() string { return "" },
	).Register(httpx.Mounts{API: mount, APIMutating: mount})

	req := httptest.NewRequest(http.MethodPost, "/api/deliverables/"+stored.ID+"/reject", nil)
	reqCtx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: teammate})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req.WithContext(httpx.WithOrgID(reqCtx, orgID)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reject as the teammate = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rs.closeState != "closed" {
		t.Fatalf("ClosePR sent state=%q, want closed", rs.closeState)
	}

	var status string
	var closeReason sql.NullString
	if err := h.AdminDB.QueryRow(`SELECT status, close_reason FROM tasks WHERE id = $1`, taskID).Scan(&status, &closeReason); err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if status != "done" || closeReason.String != db.RunCompletedCloseReason {
		t.Errorf("task = (%s, %q), want (done, %q): the teammate resolved the last deliverable of a completed run",
			status, closeReason.String, db.RunCompletedCloseReason)
	}
}
