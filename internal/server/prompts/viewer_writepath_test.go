package prompts

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
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// viewerRig is a Postgres-backed prompts handler, with RLS live, over one team
// with an admin (founder), a plain member and a viewer. Handlers are called
// directly with the claims and active org withSession would supply.
type viewerRig struct {
	h      *pgtest.Harness
	ph     *Handler
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

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")
	viewer := pgtest.SeedUser(t, h, "viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamID, "member", "viewer")

	return &viewerRig{
		h:      h,
		ph:     New(h.AdminDB, stores.Tx, authz.New(h.AdminDB, stores.Tx)),
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
	body := map[string]string{"name": "edit-me", "body": "rewritten", "model": domain.ModelSonnet}

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

// TestViewer_PromptCreate_Forbidden: a viewer's prompt create is a 403 at the
// acting-team write gate, before the create transaction; a member's is a 201.
func TestViewer_PromptCreate_Forbidden(t *testing.T) {
	r := newViewerRig(t)
	// Sole-team callers omit team_id; the resolver falls back to their one team.
	body := map[string]string{"name": "new-prompt", "body": "mission", "model": domain.ModelSonnet}

	rec := httptest.NewRecorder()
	r.ph.handlePromptCreate(rec, r.req(http.MethodPost, "/api/prompts", r.viewer, body))
	assertViewOnly403(t, rec, "viewer POST prompt")

	rec = httptest.NewRecorder()
	r.ph.handlePromptCreate(rec, r.req(http.MethodPost, "/api/prompts", r.member, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member POST prompt: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}
