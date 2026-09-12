// The two boundary cases that only exist in multi mode: the team archive, and
// the user→user handoff that deliberately is not a boundary at all.

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

// TestTeamArchive_EndsEveryConversationItEnumerated: archiving a team ends its
// work, and the stamp is over the whole enumerated set rather than only the
// conversations a stop happened to reach. This rig wires no spawner, so
// nothing is stopped at all — which is exactly the case that matters: a
// conversation left un-ended because its executor was unreachable is the one
// that would stay resumable on a team nobody can see.
//
// origin='interactive' sidesteps the origin-requires-parents CHECK; the
// enumeration reads team_id and status and nothing else.
func TestTeamArchive_EndsEveryConversationItEnumerated(t *testing.T) {
	r := newTeamArchiveRig(t)

	seed := func(status any) string {
		t.Helper()
		id := uuid.New().String()
		pgtest.MustExec(t, r.h.AdminDB, `
			INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility,
			                           type, origin, trigger_type, status)
			VALUES ($1, $2, $3, $4, 'team', 'delegation', 'interactive', 'manual', $5)
		`, id, r.orgID, r.owner, r.teamID, status)
		return id
	}
	midFlight := seed(nil)
	parked := seed("open")
	terminal := seed("completed")

	rec := httptest.NewRecorder()
	r.th.handleTeamArchive(rec, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	for _, id := range []string{midFlight, parked} {
		if ended, reason := pgConversationBoundary(t, r.h, id); !ended || reason != string(domain.EndedTeamArchived) {
			t.Errorf("%s boundary = (ended=%v, reason=%q), want a stamp with reason %q",
				id, ended, reason, domain.EndedTeamArchived)
		}
	}
	// A conversation that had already reached a terminal is not in the
	// enumerated set, so the archive says nothing about it — the same shape as
	// the stop loop it follows.
	if ended, reason := pgConversationBoundary(t, r.h, terminal); ended || reason != "" {
		t.Errorf("terminal conversation %s boundary = (ended=%v, reason=%q), want none — it was never enumerated",
			terminal, ended, reason)
	}
}

// TestTaskReassign_StampsNothing: a user→user handoff is not a boundary. The
// task does not move on — the same work continues under a new owner — so the
// conversation it is about is still the one it is about, and a stamp would
// tell the next reader the work restarted when it did not.
func TestTaskReassign_StampsNothing(t *testing.T) {
	r := newViewerRig(t)

	var entityID, evtID, taskID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title)
		VALUES ($1, 'github', $2, 'pr', 'test pr') RETURNING id
	`, r.orgID, "octo/reassign#"+t.Name()).Scan(&entityID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
	`, r.orgID, entityID).Scan(&evtID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, claimed_by_user_id)
		VALUES ($1, $2, $3, $4, 'github:pr:opened', $5, $2) RETURNING id
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

// TestTeamArchive_RingsTheMemoryDoorbellForEveryConversationItStamped: an
// archived team's runs are the clearest case for generating a memory from the
// transcript — nobody is going back to write one by hand — so each stamp rings.
//
// Once per stamped row and no more: the loop re-asks the door per conversation,
// and a row that had already ended comes back nil, with whatever it owes rung
// for by the boundary that actually ended it.
func TestTeamArchive_RingsTheMemoryDoorbellForEveryConversationItStamped(t *testing.T) {
	r := newTeamArchiveRig(t)
	d := &doorbell{}
	r.th.memoryOwed = d.ring

	seed := func(status any) string {
		t.Helper()
		id := uuid.New().String()
		pgtest.MustExec(t, r.h.AdminDB, `
			INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility,
			                           type, origin, trigger_type, status)
			VALUES ($1, $2, $3, $4, 'team', 'delegation', 'interactive', 'manual', $5)
		`, id, r.orgID, r.owner, r.teamID, status)
		return id
	}
	midFlight := seed(nil)
	parked := seed("open")
	seed("completed") // terminal: never enumerated, so never stamped and never rung

	rec := httptest.NewRecorder()
	r.th.handleTeamArchive(rec, r.req(http.MethodPost, "/api/teams/"+r.teamID+"/archive", r.owner, r.teamID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got := d.taken()
	want := map[string]bool{midFlight: true, parked: true}
	if len(got) != len(want) {
		t.Fatalf("rang %d times for %d stamped conversations: %v", len(got), len(want), got)
	}
	for _, ring := range got {
		if ring[0] != r.orgID {
			t.Errorf("rang for org %q, want %q", ring[0], r.orgID)
		}
		if !want[ring[1]] {
			t.Errorf("rang for %s, which the archive did not stamp", ring[1])
		}
		delete(want, ring[1])
	}
	for id := range want {
		t.Errorf("archive stamped %s without ringing for it", id)
	}
}
