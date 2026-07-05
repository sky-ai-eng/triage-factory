package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleSwipe_ReassignHappyPath pins the TFAC-561 user↔user handoff arm's
// core CAS behavior against local-mode SQLite: a task claimed by another user
// gets reassigned to a third user, the claim columns land as expected, and
// the audit trail records the gesture.
func TestHandleSwipe_ReassignHappyPath(t *testing.T) {
	s := newTestServer(t)
	const eventType = "github:pr:opened"
	const fromUser = "00000000-0000-0000-0000-0000000000f1"
	const toUser = "00000000-0000-0000-0000-0000000000f2"
	for _, u := range []string{fromUser, toUser} {
		if _, err := s.db.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'Seed User')`, u); err != nil {
			t.Fatalf("seed user %s: %v", u, err)
		}
	}
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_re', 'github', 'sky/repo#re', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_re', 'e_re', ?, '')`,
		eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t_re', 'e_re', ?, 'ev_re', 'in_progress', ?)`,
		eventType, fromUser,
	); err != nil {
		t.Fatalf("seed claimed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_re/swipe",
		map[string]any{"action": "reassign", "target_user_id": toUser})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status string
	var claimedUser, claimedAgent sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_user_id, claimed_by_agent_id FROM tasks WHERE id = 't_re'`,
	).Scan(&status, &claimedUser, &claimedAgent); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if !claimedUser.Valid || claimedUser.String != toUser {
		t.Errorf("claimed_by_user_id = %v, want %q", claimedUser, toUser)
	}
	if claimedAgent.Valid {
		t.Errorf("claimed_by_agent_id = %q, want empty", claimedAgent.String)
	}
	// Lifecycle status must survive the reassign untouched — reassign is
	// responsibility-axis only, same as claim/delegate.
	if status != "in_progress" {
		t.Errorf("status = %q, want preserved 'in_progress'", status)
	}

	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't_re' AND action = 'reassign'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 1 {
		t.Errorf("reassign swipe_events count = %d, want 1", swipeCount)
	}
}

// TestHandleSwipe_ReassignRefusalReasons covers the guard order documented on
// swipeReassign: missing target (400), missing task (404), terminal task
// (409), and a task that isn't currently held by a user — unclaimed or
// bot-claimed (409, since those go through claim/takeover instead).
func TestHandleSwipe_ReassignRefusalReasons(t *testing.T) {
	t.Run("missing_target_user_id_400", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/swipe",
			map[string]any{"action": "reassign"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing_task_404", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/swipe",
			map[string]any{"action": "reassign", "target_user_id": runmode.LocalDefaultUserID})
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("terminal_task_409", func(t *testing.T) {
		s := newTestServer(t)
		const otherUserID = "00000000-0000-0000-0000-0000000000f3"
		if _, err := s.db.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'Other')`, otherUserID); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO entities (id, source, source_id, kind, state)
			 VALUES ('e_ret', 'github', 'sky/repo#ret', 'pr', 'active')`,
		); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_ret', 'e_ret', 'github:pr:opened', '')`,
		); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
			 VALUES ('t_ret', 'e_ret', 'github:pr:opened', 'ev_ret', 'done', ?)`,
			runmode.LocalDefaultUserID,
		); err != nil {
			t.Fatalf("seed terminal claimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_ret/swipe",
			map[string]any{"action": "reassign", "target_user_id": otherUserID})
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unclaimed_task_409", func(t *testing.T) {
		s := newTestServer(t)
		if _, err := s.db.Exec(
			`INSERT INTO entities (id, source, source_id, kind, state)
			 VALUES ('e_reu', 'github', 'sky/repo#reu', 'pr', 'active')`,
		); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_reu', 'e_reu', 'github:pr:opened', '')`,
		); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
			 VALUES ('t_reu', 'e_reu', 'github:pr:opened', 'ev_reu', 'queued')`,
		); err != nil {
			t.Fatalf("seed unclaimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_reu/swipe",
			map[string]any{"action": "reassign", "target_user_id": runmode.LocalDefaultUserID})
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bot_claimed_task_409", func(t *testing.T) {
		s := newTestServer(t)
		if _, err := s.db.Exec(
			`INSERT INTO entities (id, source, source_id, kind, state)
			 VALUES ('e_reb', 'github', 'sky/repo#reb', 'pr', 'active')`,
		); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_reb', 'e_reb', 'github:pr:opened', '')`,
		); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_agent_id)
			 VALUES ('t_reb', 'e_reb', 'github:pr:opened', 'ev_reb', 'queued', ?)`,
			runmode.LocalDefaultAgentID,
		); err != nil {
			t.Fatalf("seed bot-claimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_reb/swipe",
			map[string]any{"action": "reassign", "target_user_id": runmode.LocalDefaultUserID})
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestHandleSwipe_ReassignIdempotentToCurrentClaimant pins the no-op path:
// reassigning a task to the user who already holds it must succeed (200)
// without disturbing the claim, mirroring swipeClaim's same-user idempotency.
func TestHandleSwipe_ReassignIdempotentToCurrentClaimant(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_rei', 'github', 'sky/repo#rei', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_rei', 'e_rei', 'github:pr:opened', '')`,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t_rei', 'e_rei', 'github:pr:opened', 'ev_rei', 'queued', ?)`,
		runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed claimed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_rei/swipe",
		map[string]any{"action": "reassign", "target_user_id": runmode.LocalDefaultUserID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = 't_rei'`,
	).Scan(&claimedUser); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !claimedUser.Valid || claimedUser.String != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %v, want unchanged %q", claimedUser, runmode.LocalDefaultUserID)
	}
}

// TestSwipeReassign_PermissionModel pins TFAC-561's "claimant + team admin"
// permission rule against real Postgres RLS (local mode has no admin concept
// to test against — callerIsAdminOfTaskTeam short-circuits true there by
// design). A plain team member who is neither the current claimant nor a team
// admin gets swipeReassign's own 403 (distinct from the broader view-only
// 403 RequireTaskWrite already covers for non-members); the team-admin
// override and the current claimant's own handoff both succeed.
func TestSwipeReassign_PermissionModel(t *testing.T) {
	r := newViewerRig(t)
	bystander := pgtest.SeedUser(t, r.h, "bystander")
	pgtest.AddOrgMember(t, r.h, bystander, r.orgID, r.teamID, "member", "member")

	seedClaimedTask := func(t *testing.T, claimant string) string {
		t.Helper()
		taskID := r.seedTask(t)
		if _, err := r.h.AdminDB.Exec(
			`UPDATE tasks SET claimed_by_user_id = $2 WHERE id = $1`, taskID, claimant,
		); err != nil {
			t.Fatalf("seed claim: %v", err)
		}
		return taskID
	}

	t.Run("non_claimant_non_admin_member_refused", func(t *testing.T) {
		taskID := seedClaimedTask(t, r.member)
		body := map[string]any{"action": "reassign", "target_user_id": bystander}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/swipe", bystander, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleSwipe(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin_override_succeeds", func(t *testing.T) {
		taskID := seedClaimedTask(t, r.member)
		body := map[string]any{"action": "reassign", "target_user_id": bystander}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/swipe", r.admin, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleSwipe(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin reassign: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var claimed sql.NullString
		if err := r.h.AdminDB.QueryRow(
			`SELECT claimed_by_user_id FROM tasks WHERE id = $1`, taskID,
		).Scan(&claimed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !claimed.Valid || claimed.String != bystander {
			t.Errorf("claimed_by_user_id = %v, want %q", claimed, bystander)
		}
	})

	t.Run("current_claimant_handoff_succeeds", func(t *testing.T) {
		taskID := seedClaimedTask(t, r.member)
		body := map[string]any{"action": "reassign", "target_user_id": bystander}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/swipe", r.member, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleSwipe(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("claimant handoff: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
}
