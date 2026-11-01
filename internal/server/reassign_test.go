package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskUndo_RefusesAGestureAnotherUserHasActedPast: undo is refused with 409
// when another user acted on the task later, under real RLS, where a user can
// SELECT only their own swipe_events rows and so cannot see the later gesture.
func TestTaskUndo_RefusesAGestureAnotherUserHasActedPast(t *testing.T) {
	r := newViewerRig(t)

	var entityID string
	repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "undo")
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
		VALUES ($1, 'github', $2, 'pr', 'test pr', 'https://github.com', $3) RETURNING id
	`, r.orgID, "octo/undo#"+t.Name(), repoID).Scan(&entityID); err != nil {
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
		INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, $3, $4, 'github:pr:opened', $5) RETURNING id
	`, r.orgID, r.admin, r.teamID, entityID, evtID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	// The member's own gesture: a real claim through the route, so the audit
	// row carries their id.
	call := func(method, path, caller string, body any) *httptest.ResponseRecorder {
		t.Helper()
		req := r.req(method, path, caller, body)
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(path, "/claim"):
			r.s.handleTaskClaim(rec, req)
		case strings.HasSuffix(path, "/undo"):
			r.s.handleUndo(rec, req)
		default:
			r.s.handleTaskPatch(rec, req)
		}
		return rec
	}

	if rec := call(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.member, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("member claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The founder then closes it — a later gesture by a different user, which
	// is exactly the row the member's session cannot see.
	if rec := call(http.MethodPatch, "/api/tasks/"+taskID, r.admin,
		map[string]any{"status": "dismissed"}); rec.Code != http.StatusOK {
		t.Fatalf("admin dismiss = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec := call(http.MethodPost, "/api/tasks/"+taskID+"/undo", r.member, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("member undo = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var status string
	var closedAt sql.NullTime
	if err := r.h.AdminDB.QueryRow(
		`SELECT status, closed_at FROM tasks WHERE id = $1`, taskID,
	).Scan(&status, &closedAt); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if status != "dismissed" || !closedAt.Valid {
		t.Fatalf("task = (%q, closed_at valid=%v) after a refused undo; want the founder's close intact",
			status, closedAt.Valid)
	}
}

// TestTaskClaim_ReassignHappyPath: a task another user holds is handed to a
// third user on the task's team, leaving the lifecycle status alone and
// recording a reassign swipe event.
func TestTaskClaim_ReassignHappyPath(t *testing.T) {
	s := newTestServer(t)
	const fromUser = "00000000-0000-0000-0000-0000000000f1"
	const toUser = "00000000-0000-0000-0000-0000000000f2"
	for _, u := range []string{fromUser, toUser} {
		execSQL(t, s.db, `INSERT INTO users (id, display_name) VALUES (?, 'Seed User')`, u)
	}
	// The target must be on the task's team, or they could not see the row
	// they are handed.
	execSQL(t, s.db, `INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'member')`,
		toUser, runmode.LocalDefaultTeamID)
	taskID := seedLifecycleTask(t, s.db, "re", lifecycleTaskOpts{claimedByUserID: fromUser})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim",
		map[string]any{"target_user_id": toUser})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status string
	var claimedUser, claimedAgent sql.NullString
	if err := s.db.QueryRow(
		`SELECT status, claimed_by_user_id, claimed_by_agent_id FROM tasks WHERE id = ?`, taskID,
	).Scan(&status, &claimedUser, &claimedAgent); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if !claimedUser.Valid || claimedUser.String != toUser {
		t.Errorf("claimed_by_user_id = %v, want %q", claimedUser, toUser)
	}
	if claimedAgent.Valid {
		t.Errorf("claimed_by_agent_id = %q, want empty", claimedAgent.String)
	}
	if status != "in_progress" {
		t.Errorf("status = %q, want preserved 'in_progress'", status)
	}

	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = ? AND action = 'reassign'`, taskID,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 1 {
		t.Errorf("reassign swipe_events count = %d, want 1", swipeCount)
	}
}

// TestTaskClaim_ReassignRefusalReasons: a missing task is 404; a terminal task,
// and one no user holds (unclaimed or bot-claimed, which take the self-claim
// arm instead), are 409.
func TestTaskClaim_ReassignRefusalReasons(t *testing.T) {
	t.Run("missing_task_404", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/no-such-task/claim",
			map[string]any{"target_user_id": runmode.LocalDefaultUserID})
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	const otherUserID = "00000000-0000-0000-0000-0000000000f3"
	for _, tc := range []struct {
		name   string
		opts   lifecycleTaskOpts
		target string
	}{
		{"terminal_task_409", lifecycleTaskOpts{status: "done", claimedByUserID: runmode.LocalDefaultUserID}, otherUserID},
		{"unclaimed_task_409", lifecycleTaskOpts{}, runmode.LocalDefaultUserID},
		{"bot_claimed_task_409", lifecycleTaskOpts{claimedByAgentID: runmode.LocalDefaultAgentID}, runmode.LocalDefaultUserID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			execSQL(t, s.db, `INSERT INTO users (id, display_name) VALUES (?, 'Other')`, otherUserID)
			taskID := seedLifecycleTask(t, s.db, tc.name, tc.opts)
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim",
				map[string]any{"target_user_id": tc.target})
			if rec.Code != http.StatusConflict {
				t.Errorf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTaskClaim_RejectsBlankTarget: a blank target_user_id is a 400, not a
// second spelling of "omitted", which would read it as a self-claim and hand
// the task to the caller.
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

// TestTaskClaim_ReassignIdempotentToCurrentClaimant: handing a task to the user
// who already holds it is a 200 that leaves the claim as it was.
func TestTaskClaim_ReassignIdempotentToCurrentClaimant(t *testing.T) {
	s := newTestServer(t)
	taskID := seedLifecycleTask(t, s.db, "rei", lifecycleTaskOpts{claimedByUserID: runmode.LocalDefaultUserID})

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim",
		map[string]any{"target_user_id": runmode.LocalDefaultUserID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var claimedUser sql.NullString
	if err := s.db.QueryRow(`SELECT claimed_by_user_id FROM tasks WHERE id = ?`, taskID).Scan(&claimedUser); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !claimedUser.Valid || claimedUser.String != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %v, want unchanged %q", claimedUser, runmode.LocalDefaultUserID)
	}
}

// TestTaskClaimReassign_PermissionModel pins who may reassign under real RLS:
// the current claimant and an admin of the task's team may, a plain member who
// is neither gets 403, and a target with no relationship to the task is a 422.
func TestTaskClaimReassign_PermissionModel(t *testing.T) {
	r := newViewerRig(t)
	bystander := pgtest.SeedUser(t, r.h, "bystander")
	pgtest.AddOrgMember(t, r.h, bystander, r.orgID, r.teamID, "member", "member")

	// seedClaimedTask mints its own entity per call, keyed by the subtest name:
	// r.seedTask uses one fixed source_id, which would collide across subtests
	// on the shared rig.
	seedClaimedTask := func(t *testing.T, claimant string) string {
		t.Helper()
		var entityID string
		repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "reassign")
		if err := r.h.AdminDB.QueryRow(`
			INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
			VALUES ($1, 'github', $2, 'pr', 'test pr', 'https://github.com', $3) RETURNING id
		`, r.orgID, "octo/reassign#"+t.Name(), repoID).Scan(&entityID); err != nil {
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
			INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, claimed_by_user_id, status)
			VALUES ($1, $2, $3, $4, 'github:pr:opened', $5, $6, 'in_progress') RETURNING id
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

	// The write must not depend on the acting admin sharing a team with the
	// target. r.admin is not on teamB, where the task's team_id moves, so the
	// write must run on the admin pool: tasks_update's WITH CHECK would refuse
	// it under r.admin's own RLS session.
	t.Run("cross_team_admin_override_succeeds", func(t *testing.T) {
		teamB := pgtest.SeedTeam(t, r.h, r.orgID, "teamb-cross")
		crossTarget := pgtest.SeedUser(t, r.h, "cross-target")
		pgtest.AddOrgMember(t, r.h, crossTarget, r.orgID, teamB, "member", "member")

		taskID := seedClaimedTask(t, r.member)
		// Widen the task's visibility to teamB, so crossTarget is a legitimate
		// target although the task's owning team is r.teamID.
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
		// The response is read on the admin pool too: the row is now outside
		// r.admin's RLS visibility, so an RLS-scoped read would answer 404.
		var got struct {
			ID              string `json:"id"`
			ClaimedByUserID string `json:"claimed_by_user_id"`
			TeamID          string `json:"team_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
		}
		if got.ID != taskID || got.ClaimedByUserID != crossTarget || got.TeamID != teamB {
			t.Errorf("response = {id:%q claimed_by_user_id:%q team_id:%q}, want {%q %q %q}",
				got.ID, got.ClaimedByUserID, got.TeamID, taskID, crossTarget, teamB)
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

	// A target on none of the task's teams is 422 CROSS_TEAM_REF, distinct
	// from a lost CAS's 409, because no retry can make it succeed.
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
