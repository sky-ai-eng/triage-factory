package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// seedTaskOnArchivedLocalTeam inserts a queued task on the local team and then
// archives that team, leaving the caller no active team to claim the task
// onto. Local mode never archives its one team; the row is written raw because
// the claim doors answer the same either way.
func seedTaskOnArchivedLocalTeam(t *testing.T, s *Server, tag string) string {
	t.Helper()
	const eventType = "github:pr:opened"
	taskID := uuid.NewString()
	repoID := dbseed.GitHubRepository(t, s.db, runmode.LocalDefaultOrgID, "https://github.com", "sky", "repo")
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO entities (id, source, source_id, kind, state, scope, repository_id) VALUES (?, 'github', ?, 'pr', 'active', 'https://github.com', ?)`,
			[]any{"e_" + tag, "sky/repo#" + tag, repoID}},
		{`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`,
			[]any{"ev_" + tag, "e_" + tag, eventType}},
		{`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status, team_id) VALUES (?, ?, ?, ?, 'queued', ?)`,
			[]any{taskID, "e_" + tag, eventType, "ev_" + tag, runmode.LocalDefaultTeamID}},
		{`UPDATE teams SET deleted_at = ? WHERE id = ?`,
			[]any{time.Now().UTC(), runmode.LocalDefaultTeamID}},
	} {
		if _, err := s.db.Exec(q.query, q.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q.query)
		}
	}
	return taskID
}

// assertNoClaimLanded fails unless the task is still unclaimed and queued and
// no swipe was recorded for it: a refused claim writes nothing.
func assertNoClaimLanded(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	var user, agent sql.NullString
	var status string
	if err := database.QueryRow(
		`SELECT claimed_by_user_id, claimed_by_agent_id, status FROM tasks WHERE id = ?`, taskID,
	).Scan(&user, &agent, &status); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if user.Valid || agent.Valid || status != "queued" {
		t.Errorf("task = (user %v, agent %v, status %q), want unclaimed and queued", user, agent, status)
	}
	var swipes int
	if err := database.QueryRow(`SELECT COUNT(*) FROM swipe_events WHERE task_id = ?`, taskID).Scan(&swipes); err != nil {
		t.Fatalf("count swipes: %v", err)
	}
	if swipes != 0 {
		t.Errorf("swipe_events = %d, want 0 for a refused claim", swipes)
	}
}

// TestTaskClaim_RefusedOnAnArchivedTeamWithNoActiveTeamToMoveTo: a claim with
// nowhere to land is a 403 TEAM_ARCHIVED, which no client presents as a retry,
// and writes nothing.
func TestTaskClaim_RefusedOnAnArchivedTeamWithNoActiveTeamToMoveTo(t *testing.T) {
	s := newTestServer(t)
	taskID := seedTaskOnArchivedLocalTeam(t, s, "claim-archived")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusForbidden || firstErrorReason(t, rec) != httpx.ReasonTeamArchived {
		t.Fatalf("claim = %d %s, want 403 TEAM_ARCHIVED", rec.Code, rec.Body.String())
	}
	assertNoClaimLanded(t, s.db, taskID)
}

// TestTaskDelegate_RefusedOnAnArchivedTeamWithNoActiveTeamToMoveTo: the same
// refusal on the delegate route, before the bot claim. Answering after it
// would leave a bot claim with no run behind a SPAWN_FAILED the client offers
// to retry.
func TestTaskDelegate_RefusedOnAnArchivedTeamWithNoActiveTeamToMoveTo(t *testing.T) {
	s := newTestServer(t)
	s.SetSpawner(delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, websocket.NewHub(), "haiku"))
	taskID := seedTaskOnArchivedLocalTeam(t, s, "delegate-archived")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusForbidden || firstErrorReason(t, rec) != httpx.ReasonTeamArchived {
		t.Fatalf("delegate = %d %s, want 403 TEAM_ARCHIVED", rec.Code, rec.Body.String())
	}
	assertNoClaimLanded(t, s.db, taskID)
	var runs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ?`, taskID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("blueprint_runs = %d, want 0", runs)
	}
}

// archivedOwnerRig is a Postgres-backed Server, with RLS live, for the case a
// member of an active team and an archived one meets: the archived team owns
// a task both teams can see. The bot is enabled on both teams, so the
// delegate gate passes whichever team the claim resolves to.
type archivedOwnerRig struct {
	h                *pgtest.Harness
	s                *Server
	orgID, user      string
	active, archived string
	agentID          string
}

func newArchivedOwnerRig(t *testing.T) *archivedOwnerRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	r := &archivedOwnerRig{h: h, s: New(h.AdminDB, pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey))}
	r.orgID, r.user, r.active = pgtest.SeedOrgWithUser(t, h, "claimant")
	r.archived = pgtest.SeedTeam(t, h, r.orgID, "archived")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, r.user, r.archived)
	if err := h.AdminDB.QueryRow(
		`INSERT INTO agents (org_id, display_name) VALUES ($1, 'Test Bot') RETURNING id::text`, r.orgID,
	).Scan(&r.agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for _, team := range []string{r.active, r.archived} {
		pgtest.MustExec(t, h.AdminDB,
			`INSERT INTO team_agents (team_id, agent_id, enabled) VALUES ($1, $2, TRUE)`, team, r.agentID)
	}
	return r
}

// seedTask inserts a queued task owned by the archived team, visible to both
// teams, and then archives the team.
func (r *archivedOwnerRig) seedTask(t *testing.T) string {
	t.Helper()
	var entityID, eventID, taskID string
	repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "archived-owner")
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
		VALUES ($1, 'github', $2, 'pr', 'test pr', 'https://github.com', $3) RETURNING id
	`, r.orgID, "octo/archived-owner#"+uuid.NewString()[:8], repoID).Scan(&entityID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
	`, r.orgID, entityID).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, status)
		VALUES ($1, $2, $3, $4, 'github:pr:opened', $5, 'queued') RETURNING id
	`, r.orgID, r.user, r.archived, entityID, eventID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	for _, team := range []string{r.active, r.archived} {
		pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, taskID, team)
	}
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, r.archived)
	return taskID
}

func (r *archivedOwnerRig) post(t *testing.T, handler http.HandlerFunc, path, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	vr := &viewerRig{orgID: r.orgID}
	req := vr.req(http.MethodPost, path, r.user, map[string]any{"hesitation_ms": 0})
	req.SetPathValue("id", taskID)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// claimRow reads the task's owner and claim off the admin pool.
func (r *archivedOwnerRig) claimRow(t *testing.T, taskID string) (team, user, agent string) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(`
		SELECT team_id::text, COALESCE(claimed_by_user_id::text, ''), COALESCE(claimed_by_agent_id::text, '')
		  FROM tasks WHERE id = $1`, taskID,
	).Scan(&team, &user, &agent); err != nil {
		t.Fatalf("read task: %v", err)
	}
	return team, user, agent
}

// TestTaskDelegate_MovesATaskOffAnArchivedOwnerOntoTheActiveTeam: delegating a
// task an archived team owns lands the bot claim on the caller's active team,
// so the run that follows has a team to belong to. On the request path RLS
// also hides the archived team's visibility row from the claim's team choice;
// the store suite covers the same choice without it.
func TestTaskDelegate_MovesATaskOffAnArchivedOwnerOntoTheActiveTeam(t *testing.T) {
	r := newArchivedOwnerRig(t)
	taskID := r.seedTask(t)

	rec := r.post(t, r.s.handleTaskDelegate, "/api/tasks/"+taskID+"/delegate", taskID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delegate = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if team, _, agent := r.claimRow(t, taskID); team != r.active || agent != r.agentID {
		t.Errorf("task = (team %s, agent %q), want the bot's claim on the active team %s (archived %s)",
			team, agent, r.active, r.archived)
	}
}

// TestTaskClaim_MovesATaskOffAnArchivedOwnerOntoTheActiveTeam: the person-claim
// sibling of the delegate case.
func TestTaskClaim_MovesATaskOffAnArchivedOwnerOntoTheActiveTeam(t *testing.T) {
	r := newArchivedOwnerRig(t)
	taskID := r.seedTask(t)

	rec := r.post(t, r.s.handleTaskClaim, "/api/tasks/"+taskID+"/claim", taskID)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if team, user, _ := r.claimRow(t, taskID); team != r.active || user != r.user {
		t.Errorf("task = (team %s, user %q), want the caller's claim on the active team %s (archived %s)",
			team, user, r.active, r.archived)
	}
}

// TestTaskReassign_RefusesATargetWhoseOnlyTeamOnTheTaskIsArchived: the
// handoff's write runs on the admin pool, where RLS does not hide an archived
// team's visibility row, so its own guard is what keeps a claim off an
// archived team. A target whose only team on the task is archived is on no
// team that can see it: 422, and the claim stays where it was.
func TestTaskReassign_RefusesATargetWhoseOnlyTeamOnTheTaskIsArchived(t *testing.T) {
	r := newArchivedOwnerRig(t)
	target := pgtest.SeedUser(t, r.h, "target")
	pgtest.AddOrgMember(t, r.h, target, r.orgID, r.archived, "member", "member")
	taskID := r.seedTask(t)
	// The caller holds the task on the active team, as a claim taken before
	// the archive would leave it.
	pgtest.MustExec(t, r.h.AdminDB,
		`UPDATE tasks SET team_id = $1, claimed_by_user_id = $2, status = 'in_progress' WHERE id = $3`,
		r.active, r.user, taskID)

	vr := &viewerRig{orgID: r.orgID}
	req := vr.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.user,
		map[string]any{"target_user_id": target, "hesitation_ms": 0})
	req.SetPathValue("id", taskID)
	rec := httptest.NewRecorder()
	r.s.handleTaskClaim(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || firstErrorReason(t, rec) != httpx.ReasonCrossTeamRef {
		t.Fatalf("reassign = %d %s, want 422 CROSS_TEAM_REF", rec.Code, rec.Body.String())
	}
	if team, user, _ := r.claimRow(t, taskID); team != r.active || user != r.user {
		t.Errorf("task = (team %s, user %q), want it still held by the caller on the active team %s",
			team, user, r.active)
	}
}
