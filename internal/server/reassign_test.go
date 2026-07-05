package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// toUser needs a membership on the task's team — the target-relevance
	// guard requires it (a claimed task's visibility is governed solely by
	// team_id; a target with no relationship to it would be unable to even
	// see the row afterward).
	if _, err := s.db.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'member')`,
		toUser, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed toUser membership: %v", err)
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
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, claimed_by_user_id, team_id)
		 VALUES ('t_re', 'e_re', ?, 'ev_re', 'in_progress', ?, ?)`,
		eventType, fromUser, runmode.LocalDefaultTeamID,
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
// to test against — the check short-circuits true there by design). A plain
// team member who is neither the current claimant nor an admin of the task's
// owning team gets swipeReassign's own 403 (distinct from the broader
// view-only 403 RequireTaskWrite already covers for non-members); the
// team-admin override and the current claimant's own handoff both succeed.
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

	// Regression pin: the reassign write must not depend on the acting
	// admin sharing a team with the target. r.admin admins r.teamID (the
	// task's owning team, so the permission arm passes) but is NOT a
	// member of teamB at all; crossTarget is only a member of teamB. Post-
	// reassign, team_id consolidates to teamB (crossTarget's own team) —
	// exactly the case where Postgres's tasks_update RLS WITH CHECK would
	// reject the write if it ran on the acting admin's own RLS session
	// (they can't write a team they're not in), which is why the mutation
	// must run through the admin pool (ReassignClaimToUserSystem) once
	// both permission arms are validated in Go.
	t.Run("cross_team_admin_override_succeeds", func(t *testing.T) {
		teamB := pgtest.SeedTeam(t, r.h, r.orgID, "teamb-cross")
		crossTarget := pgtest.SeedUser(t, r.h, "cross-target")
		pgtest.AddOrgMember(t, r.h, crossTarget, r.orgID, teamB, "member", "member")

		taskID := seedClaimedTask(t, r.member)
		// Widen the task's visibility to teamB (the historical "who can see
		// this" set a multi-team router would have recorded) so crossTarget
		// is a legitimate reassign target despite the task's OWNING team
		// being r.teamID.
		pgtest.MustExec(t, r.h.AdminDB,
			`INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, taskID, teamB)

		body := map[string]any{"action": "reassign", "target_user_id": crossTarget}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/swipe", r.admin, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleSwipe(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("cross-team admin override: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var claimed, gotTeam sql.NullString
		if err := r.h.AdminDB.QueryRow(
			`SELECT claimed_by_user_id, team_id FROM tasks WHERE id = $1`, taskID,
		).Scan(&claimed, &gotTeam); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !claimed.Valid || claimed.String != crossTarget {
			t.Errorf("claimed_by_user_id = %v, want %q", claimed, crossTarget)
		}
		if !gotTeam.Valid || gotTeam.String != teamB {
			t.Errorf("team_id after cross-team reassign = %v, want %q (target's own team)", gotTeam, teamB)
		}
	})

	// Regression pin: a target with zero relationship to the task's team(s)
	// is refused with the target-relevance message, not the generic
	// race-lost message (retrying would never succeed).
	t.Run("target_with_no_relevant_team_refused", func(t *testing.T) {
		unrelated := pgtest.SeedUser(t, r.h, "unrelated-target")
		taskID := seedClaimedTask(t, r.member)
		body := map[string]any{"action": "reassign", "target_user_id": unrelated}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/swipe", r.member, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleSwipe(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "isn't on a team") {
			t.Errorf("body=%s; want the target-relevance message, not the generic race message", rec.Body.String())
		}
	})
}
