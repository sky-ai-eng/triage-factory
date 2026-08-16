package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestRequireOrg_MultiModeNoActiveOrg_Returns409 pins the 409 contract:
// in multi mode, a session whose active_org_id is NULL flows through
// withSession with ClaimsFrom set but OrgIDFrom returning empty.
// requireOrg must reject the handler with a 409 carrying the stable
// NO_ACTIVE_ORG reason so the SPA can prompt the user to pick or
// join an org. The local-mode shim guarantees a non-empty orgID so this
// branch never fires there; we exercise the empty path directly by
// constructing a context without ctxKeyOrgID set.
func TestRequireOrg_MultiModeNoActiveOrg_Returns409(t *testing.T) {
	s := &Server{}

	// Multi-mode-shaped context: claims present, no org.
	ctx := httpx.WithClaims(context.Background(), &verify.Claims{Subject: "user-with-no-org"})

	r := httptest.NewRequest("POST", "/api/tasks/list", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	orgID, ok := s.requireOrg(rec, r)
	if ok {
		t.Fatalf("requireOrg returned ok=true with empty orgID, want false")
	}
	if orgID != "" {
		t.Errorf("requireOrg returned orgID = %q, want empty", orgID)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Errors []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Reason != httpx.ReasonNoActiveOrg {
		t.Errorf("errors = %+v, want one NO_ACTIVE_ORG item", body.Errors)
	}
	if len(body.Errors) == 1 && body.Errors[0].Message == "" {
		t.Error("message field is empty; should describe how to pick an org")
	}
	if body.Error == "" {
		t.Error("legacy error key is empty; the dual-key shim should mirror the first message")
	}
}

// TestRequireOrg_OrgPresent_ReturnsValue pins the happy path: when
// withSession has populated ctxKeyOrgID (either from the local shim
// sentinel or the multi-mode session's active_org_id), requireOrg
// returns the value untouched and ok=true.
func TestRequireOrg_OrgPresent_ReturnsValue(t *testing.T) {
	s := &Server{}
	ctx := httpx.WithOrgID(context.Background(), "00000000-0000-0000-0000-000000000abc")
	r := httptest.NewRequest("POST", "/api/tasks/list", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	orgID, ok := s.requireOrg(rec, r)
	if !ok {
		t.Fatalf("requireOrg returned ok=false with orgID present")
	}
	if orgID != "00000000-0000-0000-0000-000000000abc" {
		t.Errorf("orgID = %q, want sentinel", orgID)
	}
	if rec.Code != 0 && rec.Code != http.StatusOK {
		t.Errorf("status = %d, want default (handler should be free to write later)", rec.Code)
	}
}
