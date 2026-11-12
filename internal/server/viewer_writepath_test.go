package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// viewerRig is a Postgres-backed Server, with RLS live, over one team with an
// admin (founder), a plain member and a viewer. Handlers are called directly
// with the claims and active org withSession would supply.
type viewerRig struct {
	h      *pgtest.Harness
	s      *Server
	eh     *eventHandlersHandler
	orgID  string
	teamID string
	admin  string
	member string
	viewer string
}

func newViewerRig(t *testing.T) *viewerRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")
	viewer := pgtest.SeedUser(t, h, "viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamID, "member", "viewer")

	return &viewerRig{
		h:      h,
		s:      s,
		eh:     &eventHandlersHandler{tx: s.tx, az: s.az, catalogs: s},
		orgID:  orgID,
		teamID: teamID,
		admin:  founder,
		member: member,
		viewer: viewer,
	}
}

// bindGitHubPAT sets the org's GitHub URL and stores an org GitHub PAT under
// the founder's claims, making the github event source available to the rig's
// org.
func (r *viewerRig) bindGitHubPAT(t *testing.T) {
	t.Helper()
	if err := r.s.tx.WithTx(t.Context(), r.orgID, r.admin, func(tx db.TxStores) error {
		if _, err := tx.Orgs.SetSourceBaseURL(t.Context(), r.orgID, eventsource.KindGitHub, "https://github.com"); err != nil {
			return err
		}
		return integrations.Save(t.Context(), tx.Secrets, r.orgID, auth.Credentials{
			GitHubPAT: "ghp-fixture",
		})
	}); err != nil {
		t.Fatalf("bind github pat: %v", err)
	}
}

// req builds a request to path as callerID with claims + active org injected.
func (r *viewerRig) req(method, path, callerID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

// seedTask inserts entity + event + a team-visible task on the rig's team.
func (r *viewerRig) seedTask(t *testing.T) string {
	t.Helper()
	var entityID string
	repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "repo")
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
		VALUES ($1, 'github', 'octo/repo#1', 'pr', 'test pr', 'https://github.com', $2) RETURNING id
	`, r.orgID, repoID).Scan(&entityID); err != nil {
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
	return taskID
}

func assertViewOnly403(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s: status = %d, want 403; body=%s", what, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "view-only") {
		t.Errorf("%s: body = %s, want a view-only message", what, rec.Body.String())
	}
}

// TestViewer_EventHandlerCreate_Forbidden: a viewer POSTing a task rule 403s at
// the acting-team gate; a member creates (201). Covers the event-handler gates
// added alongside the RLS swap.
func TestViewer_EventHandlerCreate_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	// The create route refuses an event type whose source the org cannot
	// produce, and that check runs with the rest of the body validation —
	// before the role gate. Bind the credential so the request reaches the
	// gate this test is about.
	r.bindGitHubPAT(t)
	body := map[string]any{"event_type": "github:pr:opened", "name": "my-rule"}

	rec := httptest.NewRecorder()
	r.eh.handleEventHandlerCreateRule(rec, r.req(http.MethodPost, "/api/event-handlers/rules", r.viewer, body))
	assertViewOnly403(t, rec, "viewer POST event-handler")

	rec = httptest.NewRecorder()
	r.eh.handleEventHandlerCreateRule(rec, r.req(http.MethodPost, "/api/event-handlers/rules", r.member, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member POST event-handler: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestViewer_TaskPatch_Forbidden: a viewer dismissing a team task 403s; a
// member is not 403'd (the gate keys on role, not membership).
func TestViewer_TaskPatch_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	taskID := r.seedTask(t)
	body := map[string]any{"status": "dismissed"}

	rec := httptest.NewRecorder()
	req := r.req(http.MethodPatch, "/api/tasks/"+taskID, r.viewer, body)
	req.SetPathValue("id", taskID)
	r.s.handleTaskPatch(rec, req)
	assertViewOnly403(t, rec, "viewer dismiss")

	// Member is not blocked by the role gate.
	rec = httptest.NewRecorder()
	req = r.req(http.MethodPatch, "/api/tasks/"+taskID, r.member, body)
	req.SetPathValue("id", taskID)
	r.s.handleTaskPatch(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("member dismiss: got 403 (role gate must allow members); body=%s", rec.Body.String())
	}
}

// TestViewer_TaskClaim_Forbidden: the same gate on the claim verb, which
// reaches the store through a different path than the field write.
func TestViewer_TaskClaim_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	taskID := r.seedTask(t)

	rec := httptest.NewRecorder()
	req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.viewer, map[string]any{})
	req.SetPathValue("id", taskID)
	r.s.handleTaskClaim(rec, req)
	assertViewOnly403(t, rec, "viewer claim")

	rec = httptest.NewRecorder()
	req = r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.member, map[string]any{})
	req.SetPathValue("id", taskID)
	r.s.handleTaskClaim(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("member claim: got 403 (role gate must allow members); body=%s", rec.Body.String())
	}
}

// TestViewer_TaskCreate_Forbidden: a viewer hitting POST /api/tasks 403s at the
// acting-team write gate, before any task is synthesized. A member's request
// passes the gate (and proceeds to fail on the bogus entity, not 403).
func TestViewer_TaskCreate_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	body := map[string]string{
		"entity_id":  "00000000-0000-0000-0000-000000000001",
		"event_type": "github:pr:opened",
	}

	rec := httptest.NewRecorder()
	r.s.handleTaskCreate(rec, r.req(http.MethodPost, "/api/tasks", r.viewer, body))
	assertViewOnly403(t, rec, "viewer task create")

	// Member passes the role gate — they get a non-403 (404 for the bogus
	// entity), proving the gate doesn't block writers.
	rec = httptest.NewRecorder()
	r.s.handleTaskCreate(rec, r.req(http.MethodPost, "/api/tasks", r.member, body))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("member task create: got 403 (role gate must allow members); body=%s", rec.Body.String())
	}
}

// TestViewer_TaskGet_CanWrite: the task detail read answers can_write from the
// check the task write routes run, so the run station offers "Return to
// queue" to the team's writers and not to its viewer.
func TestViewer_TaskGet_CanWrite(t *testing.T) {
	r := newViewerRig(t)
	taskID := r.seedTask(t)

	for _, tc := range []struct {
		name, caller string
		want         bool
	}{
		{"admin", r.admin, true},
		{"member", r.member, true},
		{"viewer", r.viewer, false},
	} {
		rec := httptest.NewRecorder()
		req := r.req(http.MethodGet, "/api/tasks/"+taskID, tc.caller, nil)
		req.SetPathValue("id", taskID)
		r.s.handleTaskGet(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET task: status = %d, want 200; body=%s", tc.name, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s GET task: decode: %v", tc.name, err)
		}
		if body["can_write"] != tc.want {
			t.Errorf("%s GET task: can_write = %v, want %v", tc.name, body["can_write"], tc.want)
		}
	}
}

// TestViewer_TaskList_CanWrite: each row of the task list carries the write
// gate's answer, from the one predicate the detail read's probe runs, so the
// board offers the assignee picker and the drag only to a caller the claim and
// requeue routes accept. A team task is its team's writers'; an org-visible
// one is an org admin's; a writer of a team the task shares through task_teams
// may write it only while it is unclaimed, as tasks_update says.
func TestViewer_TaskList_CanWrite(t *testing.T) {
	r := newViewerRig(t)
	sideTeam := pgtest.SeedTeam(t, r.h, r.orgID, "side-team")
	split := pgtest.SeedUser(t, r.h, "split")
	pgtest.AddOrgMember(t, r.h, split, r.orgID, r.teamID, "member", "viewer")
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, split, sideTeam)

	teamTask := r.seedTask(t)
	// copyTask seeds another task on teamTask's entity, kept apart from it by
	// dedup_key under the one-active-task index.
	copyTask := func(dedup, visibility, status string, claimedBy any) string {
		t.Helper()
		var id string
		if err := r.h.AdminDB.QueryRow(`
			INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id,
			                   dedup_key, visibility, status, claimed_by_user_id)
			SELECT org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, $2, $3, $4, $5
			  FROM tasks WHERE id = $1
			RETURNING id
		`, teamTask, dedup, visibility, status, claimedBy).Scan(&id); err != nil {
			t.Fatalf("seed task %s: %v", dedup, err)
		}
		return id
	}
	orgTask := copyTask("org-copy", "org", "queued", nil)
	unclaimedShared := copyTask("unclaimed-shared", "team", "queued", nil)
	claimedShared := copyTask("claimed-shared", "team", "in_progress", r.member)
	for _, id := range []string{unclaimedShared, claimedShared} {
		pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, id, sideTeam)
	}

	listed := func(caller string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		r.s.handleTaskList(rec, r.req(http.MethodPost, "/api/tasks/list", caller, map[string]any{}))
		if rec.Code != http.StatusOK {
			t.Fatalf("list tasks as %s = %d; body=%s", caller, rec.Code, rec.Body.String())
		}
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		out := map[string]any{}
		for _, row := range page.Items {
			out[row["id"].(string)] = row["can_write"]
		}
		return out
	}
	detail := func(caller, taskID string) any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := r.req(http.MethodGet, "/api/tasks/"+taskID, caller, nil)
		req.SetPathValue("id", taskID)
		r.s.handleTaskGet(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET task %s as %s = %d; body=%s", taskID, caller, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode task: %v", err)
		}
		return body["can_write"]
	}

	for _, tc := range []struct {
		name, caller string
		team, org    bool
		// The answers on the task shared with the side team, unclaimed and
		// claimed.
		unclaimed, claimed bool
	}{
		{"admin", r.admin, true, true, true, true},
		{"member", r.member, true, false, true, true},
		{"viewer", r.viewer, false, false, false, false},
		{"team viewer, side-team writer", split, false, false, true, false},
	} {
		rows := listed(tc.caller)
		for _, task := range []struct {
			label, id string
			want      bool
		}{
			{"team task", teamTask, tc.team},
			{"org task", orgTask, tc.org},
			{"unclaimed shared task", unclaimedShared, tc.unclaimed},
			{"claimed shared task", claimedShared, tc.claimed},
		} {
			got, ok := rows[task.id]
			if !ok {
				t.Errorf("%s: %s missing from the list", tc.name, task.label)
				continue
			}
			if got != task.want {
				t.Errorf("%s: %s list can_write = %v, want %v", tc.name, task.label, got, task.want)
			}
			if d := detail(tc.caller, task.id); d != got {
				t.Errorf("%s: %s detail can_write = %v, list = %v; want one answer", tc.name, task.label, d, got)
			}
		}
	}
}
