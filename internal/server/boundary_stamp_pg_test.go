// The multi-mode user→user handoff, which deliberately is not a boundary at
// all.

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// pgConversationBoundary reads one conversation's stamp off the admin pool.
func pgConversationBoundary(t *testing.T, h *pgtest.Harness, conversationID string) (ended bool, reason string) {
	t.Helper()
	if err := h.AdminDB.QueryRow(
		`SELECT ended_at IS NOT NULL, COALESCE(ended_reason, '') FROM conversations WHERE id = $1`,
		conversationID,
	).Scan(&ended, &reason); err != nil {
		t.Fatalf("read boundary of %s: %v", conversationID, err)
	}
	return ended, reason
}

// TestTaskReassign_StampsNothing: a user→user handoff is not a boundary. The
// same work continues under a new owner, so a stamp would tell the next reader
// the work restarted when it did not.
func TestTaskReassign_StampsNothing(t *testing.T) {
	r := newViewerRig(t)

	var entityID, evtID, taskID string
	repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "reassign")
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
		VALUES ($1, 'github', $2, 'pr', 'test pr', 'https://github.com', $3) RETURNING id
	`, r.orgID, "octo/reassign#"+t.Name(), repoID).Scan(&entityID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
	`, r.orgID, entityID).Scan(&evtID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, claimed_by_user_id, status)
		VALUES ($1, $2, $3, $4, 'github:pr:opened', $5, $2, 'in_progress') RETURNING id
	`, r.orgID, r.admin, r.teamID, entityID, evtID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	conversationID := uuid.New().String()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility,
		                           type, origin, trigger_type, status, task_id)
		VALUES ($1, $2, $3, $4, 'team', 'delegation', 'interactive', 'manual', 'open', $5)
	`, conversationID, r.orgID, r.admin, r.teamID, taskID)

	req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.admin,
		map[string]any{"target_user_id": r.member, "hesitation_ms": 0})
	req.SetPathValue("id", taskID)
	rec := httptest.NewRecorder()
	r.s.handleTaskClaim(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reassign = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var claimant string
	if err := r.h.AdminDB.QueryRow(`SELECT COALESCE(claimed_by_user_id::text, '') FROM tasks WHERE id = $1`, taskID).Scan(&claimant); err != nil {
		t.Fatalf("read back the claim: %v", err)
	}
	if claimant != r.member {
		t.Fatalf("claimed_by_user_id = %q, want the handoff target %q", claimant, r.member)
	}
	if ended, reason := pgConversationBoundary(t, r.h, conversationID); ended || reason != "" {
		t.Errorf("%s boundary = (ended=%v, reason=%q), want none — a handoff is not a boundary",
			conversationID, ended, reason)
	}
}
