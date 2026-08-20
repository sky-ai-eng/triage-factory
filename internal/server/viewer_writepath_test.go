package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// viewerRig is the multi-mode fixture for the viewer-role write-path tests
// (TFAC-447): a real Postgres-backed Server (RLS live) with one team carrying
// an admin (founder), a plain member, and a viewer. The handlers are exercised
// directly with injected claims + active-org context, the same shape
// withSession/withOrg supply in production.
type viewerRig struct {
	h      *pgtest.Harness
	s      *Server
	ph     *promptsHandler
	bh     *blueprintsHandler
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
		ph:     &promptsHandler{db: h.AdminDB, tx: s.tx, az: s.az},
		bh:     &blueprintsHandler{tx: s.tx, az: s.az, spawner: func() *delegate.Spawner { return nil }},
		eh:     &eventHandlersHandler{tx: s.tx, az: s.az},
		orgID:  orgID,
		teamID: teamID,
		admin:  founder,
		member: member,
		viewer: viewer,
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

// seedPrompt inserts a team prompt via the admin pool and returns its id.
func (r *viewerRig) seedPrompt(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
		VALUES ($1, $2, $3, $4, 'mission', 'user') RETURNING id
	`, r.orgID, r.admin, r.teamID, name).Scan(&id); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return id
}

// seedTask inserts entity + event + a team-visible task on the rig's team.
func (r *viewerRig) seedTask(t *testing.T) string {
	t.Helper()
	var entityID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title)
		VALUES ($1, 'github', 'octo/repo#1', 'pr', 'test pr') RETURNING id
	`, r.orgID).Scan(&entityID); err != nil {
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

// TestViewer_PromptEdit_Forbidden: a viewer can read a team prompt but a PUT
// 403s with the view-only message; a member's PUT succeeds. Pins both the gate
// and that it doesn't over-reach onto reads.
func TestViewer_PromptEdit_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	id := r.seedPrompt(t, "edit-me")
	body := map[string]string{"name": "edit-me", "body": "rewritten", "model": "sonnet"}

	// Viewer read succeeds.
	rec := httptest.NewRecorder()
	getReq := r.req(http.MethodGet, "/api/prompts/"+id, r.viewer, nil)
	getReq.SetPathValue("id", id)
	r.ph.handlePromptGet(rec, getReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET prompt: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Viewer PUT 403s.
	rec = httptest.NewRecorder()
	putReq := r.req(http.MethodPut, "/api/prompts/"+id, r.viewer, body)
	putReq.SetPathValue("id", id)
	r.ph.handlePromptPut(rec, putReq)
	assertViewOnly403(t, rec, "viewer PUT prompt")

	// Member PUT succeeds.
	rec = httptest.NewRecorder()
	putReq = r.req(http.MethodPut, "/api/prompts/"+id, r.member, body)
	putReq.SetPathValue("id", id)
	r.ph.handlePromptPut(rec, putReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("member PUT prompt: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestViewer_PromptCreate_Forbidden: a viewer POSTing a new prompt 403s at the
// acting-team write gate (before the create tx); a member's POST creates (201).
// Exercises the two-phase resolution the create path uses — ResolveActingNoStamp
// for the gate, then ResolveActing (with stamp) in the main tx — which is a new
// pattern worth its own integration coverage beyond the RLS layer.
func TestViewer_PromptCreate_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	// Sole-team callers omit team_id; the resolver falls back to their one team.
	body := map[string]string{"name": "new-prompt", "body": "mission", "model": "sonnet"}

	rec := httptest.NewRecorder()
	r.ph.handlePromptCreate(rec, r.req(http.MethodPost, "/api/prompts", r.viewer, body))
	assertViewOnly403(t, rec, "viewer POST prompt")

	rec = httptest.NewRecorder()
	r.ph.handlePromptCreate(rec, r.req(http.MethodPost, "/api/prompts", r.member, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member POST prompt: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestViewer_BlueprintCreate_Forbidden: a viewer POSTing a blueprint 403s at the
// acting-team gate; a member creates (201). Covers the blueprint handler gates
// added alongside the RLS swap so a viewer gets a clean 403 rather than the
// blueprints_insert RLS violation.
func TestViewer_BlueprintCreate_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	body := map[string]string{"name": "new-blueprint"}

	rec := httptest.NewRecorder()
	r.bh.handleBlueprintCreate(rec, r.req(http.MethodPost, "/api/blueprints", r.viewer, body))
	assertViewOnly403(t, rec, "viewer POST blueprint")

	rec = httptest.NewRecorder()
	r.bh.handleBlueprintCreate(rec, r.req(http.MethodPost, "/api/blueprints", r.member, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member POST blueprint: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestViewer_EventHandlerCreate_Forbidden: a viewer POSTing a task rule 403s at
// the acting-team gate; a member creates (201). Covers the event-handler gates
// added alongside the RLS swap.
func TestViewer_EventHandlerCreate_Forbidden(t *testing.T) {
	r := newViewerRig(t)
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
