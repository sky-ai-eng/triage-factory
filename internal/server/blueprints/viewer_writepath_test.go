package blueprints

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
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// viewerRig is a Postgres-backed blueprints handler, with RLS live, over one
// team with an admin (founder), a plain member and a viewer. Handlers are
// called directly with the claims and active org withSession would supply.
type viewerRig struct {
	bh     *Handler
	orgID  string
	member string
	viewer string
}

func newViewerRig(t *testing.T) *viewerRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")
	viewer := pgtest.SeedUser(t, h, "viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamID, "member", "viewer")

	return &viewerRig{
		bh:     New(stores.Tx, authz.New(h.AdminDB, stores.Tx), func() *delegate.Spawner { return nil }),
		orgID:  orgID,
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

func assertViewOnly403(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s: status = %d, want 403; body=%s", what, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "view-only") {
		t.Errorf("%s: body = %s, want a view-only message", what, rec.Body.String())
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
