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

// TestTaskClaim_ReassignHappyPath pins the user↔user handoff arm's
// core CAS behavior against local-mode SQLite: a task claimed by another user
// gets reassigned to a third user, the claim columns land as expected, and
// the audit trail records the gesture.
func TestTaskClaim_ReassignHappyPath(t *testing.T) {
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
		 VALUES ('00000000-0000-4000-8000-000000000018', 'e_re', ?, 'ev_re', 'in_progress', ?, ?)`,
		eventType, fromUser, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed claimed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000018/claim",
		map[string]any{"target_user_id": toUser})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status string
	var claimedUser, claimedAgent sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_user_id, claimed_by_agent_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000018'`,
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
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = '00000000-0000-4000-8000-000000000018' AND action = 'reassign'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 1 {
		t.Errorf("reassign swipe_events count = %d, want 1", swipeCount)
	}
}

// TestTaskClaim_ReassignRefusalReasons covers the guard order documented on
// reassignClaim: missing task (404), terminal task (409), and a task that
// isn't currently held by a user — unclaimed or bot-claimed (409, since those
// go through the self-claim arm instead).
func TestTaskClaim_ReassignRefusalReasons(t *testing.T) {
	t.Run("missing_task_404", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/claim",
			map[string]any{"target_user_id": runmode.LocalDefaultUserID})
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
			 VALUES ('00000000-0000-4000-8000-000000000016', 'e_ret', 'github:pr:opened', 'ev_ret', 'done', ?)`,
			runmode.LocalDefaultUserID,
		); err != nil {
			t.Fatalf("seed terminal claimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000016/claim",
			map[string]any{"target_user_id": otherUserID})
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
			 VALUES ('00000000-0000-4000-8000-000000000017', 'e_reu', 'github:pr:opened', 'ev_reu', 'queued')`,
		); err != nil {
			t.Fatalf("seed unclaimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000017/claim",
			map[string]any{"target_user_id": runmode.LocalDefaultUserID})
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
			 VALUES ('00000000-0000-4000-8000-000000000014', 'e_reb', 'github:pr:opened', 'ev_reb', 'queued', ?)`,
			runmode.LocalDefaultAgentID,
		); err != nil {
			t.Fatalf("seed bot-claimed task: %v", err)
		}
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000014/claim",
			map[string]any{"target_user_id": runmode.LocalDefaultUserID})
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestTaskClaim_RejectsBlankTarget pins that an explicitly blank
// target_user_id is a fault, not a second spelling of "omitted". The field's
// PRESENCE is what selects the handoff arm; a client that sends a blank id has
// lost track of its target, and reading it as a self-claim would hand the task
// to the caller instead of saying so.
func TestTaskClaim_RejectsBlankTarget(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "blank-target", lifecycleTaskOpts{})

	for _, blank := range []string{"", "   "} {
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim",
			map[string]any{"target_user_id": blank})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("target_user_id=%q: status = %d, want 400; body=%s", blank, rec.Code, rec.Body.String())
		}
	}
	// Omitting it entirely is the self-claim, and still works.
	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("self-claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTaskClaim_ReassignIdempotentToCurrentClaimant pins the no-op path:
// handing a task to the user who already holds it must succeed (200)
// without disturbing the claim, mirroring the self-claim arm's idempotency.
func TestTaskClaim_ReassignIdempotentToCurrentClaimant(t *testing.T) {
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
		 VALUES ('00000000-0000-4000-8000-000000000015', 'e_rei', 'github:pr:opened', 'ev_rei', 'queued', ?)`,
		runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed claimed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000015/claim",
		map[string]any{"target_user_id": runmode.LocalDefaultUserID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var claimedUser sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000015'`,
	).Scan(&claimedUser); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !claimedUser.Valid || claimedUser.String != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %v, want unchanged %q", claimedUser, runmode.LocalDefaultUserID)
	}
}

// TestTaskClaimReassign_PermissionModel pins the "claimant + team admin"
// permission rule against real Postgres RLS (local mode has no admin concept
// to test against — the check short-circuits true there by design). A plain
// team member who is neither the current claimant nor an admin of the task's
// owning team gets reassignClaim's own 403 (distinct from the broader
// view-only 403 RequireTaskWrite already covers for non-members); the
// team-admin override and the current claimant's own handoff both succeed.
func TestTaskClaimReassign_PermissionModel(t *testing.T) {
	r := newViewerRig(t)
	bystander := pgtest.SeedUser(t, r.h, "bystander")
	pgtest.AddOrgMember(t, r.h, bystander, r.orgID, r.teamID, "member", "member")

	// seedClaimedTask mints its OWN entity per call (unlike r.seedTask, which
	// hardcodes a single fixed source_id — fine for its other callers, which
	// each seed at most one task per rig, but this test seeds one per
	// subtest against the same rig/org, so a fixed source_id would collide
	// on entities' (org_id, source, source_id) unique constraint after the
	// first call). t.Name() is unique per subtest, so it's a clean,
	// dependency-free uniqueness key.
	seedClaimedTask := func(t *testing.T, claimant string) string {
		t.Helper()
		var entityID string
		if err := r.h.AdminDB.QueryRow(`
			INSERT INTO entities (org_id, source, source_id, kind, title)
			VALUES ($1, 'github', $2, 'pr', 'test pr') RETURNING id
		`, r.orgID, "octo/reassign#"+t.Name()).Scan(&entityID); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		var evtID string
		if err := r.h.AdminDB.QueryRow(`
			INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
		`, r.orgID, entityID).Scan(&evtID); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		var taskID string
		if err := r.h.AdminDB.QueryRow(`
			INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, claimed_by_user_id)
			VALUES ($1, $2, $3, $4, 'github:pr:opened', $5, $6) RETURNING id
		`, r.orgID, r.admin, r.teamID, entityID, evtID, claimant).Scan(&taskID); err != nil {
			t.Fatalf("seed claimed task: %v", err)
		}
		return taskID
	}

	t.Run("non_claimant_non_admin_member_refused", func(t *testing.T) {
		taskID := seedClaimedTask(t, r.member)
		body := map[string]any{"target_user_id": bystander}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", bystander, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleTaskClaim(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin_override_succeeds", func(t *testing.T) {
		taskID := seedClaimedTask(t, r.member)
		body := map[string]any{"target_user_id": bystander}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.admin, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleTaskClaim(rec, req)
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
		body := map[string]any{"target_user_id": bystander}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.member, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleTaskClaim(rec, req)
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

		body := map[string]any{"target_user_id": crossTarget}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.admin, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleTaskClaim(rec, req)
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
	// is a reference to an object in the wrong team — 422 CROSS_TEAM_REF,
	// distinct from the 409 a genuinely lost CAS answers, because no retry
	// will ever make this one succeed.
	t.Run("target_with_no_relevant_team_refused", func(t *testing.T) {
		unrelated := pgtest.SeedUser(t, r.h, "unrelated-target")
		taskID := seedClaimedTask(t, r.member)
		body := map[string]any{"target_user_id": unrelated}
		req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.member, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		r.s.handleTaskClaim(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "CROSS_TEAM_REF") {
			t.Errorf("body=%s; want reason CROSS_TEAM_REF, not the generic race conflict", rec.Body.String())
		}
	})
}
