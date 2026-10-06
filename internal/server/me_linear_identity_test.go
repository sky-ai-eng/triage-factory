package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

type meLinearBody struct {
	LinearUserID      *string `json:"linear_user_id"`
	LinearDisplayName *string `json:"linear_display_name"`
}

func getMeLinear(t *testing.T, s *Server) meLinearBody {
	t.Helper()
	rec := httptest.NewRecorder()
	s.withSession(http.HandlerFunc(s.handleMe)).ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body meLinearBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	return body
}

// TestHandleMe_LocalMode_LinearIdentityFromOrgWorkspace: local /api/me carries
// the user's Linear binding in the org's workspace, and only that one — a
// binding in another workspace is a different Linear user id and never stands
// in for it.
func TestHandleMe_LocalMode_LinearIdentityFromOrgWorkspace(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := t.Context()

	if err := s.users.UpsertLinearIdentity(ctx, runmode.LocalDefaultUserID, "ws-elsewhere", "lin-elsewhere", "Elsewhere", "api_key"); err != nil {
		t.Fatalf("UpsertLinearIdentity(elsewhere): %v", err)
	}

	// The org has no Linear workspace: nothing to report, even though the
	// user holds a binding in some other workspace.
	if got := getMeLinear(t, s); got.LinearUserID != nil || got.LinearDisplayName != nil {
		t.Errorf("no workspace: linear_user_id=%v linear_display_name=%v, want both absent", got.LinearUserID, got.LinearDisplayName)
	}

	set, err := s.orgs.GetSettings(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	set.LinearWorkspaceID = "ws-org"
	if _, err := s.orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// The org is in a workspace the user has not bound.
	if got := getMeLinear(t, s); got.LinearUserID != nil || got.LinearDisplayName != nil {
		t.Errorf("unbound workspace: linear_user_id=%v linear_display_name=%v, want both absent", got.LinearUserID, got.LinearDisplayName)
	}

	if err := s.users.UpsertLinearIdentity(ctx, runmode.LocalDefaultUserID, "ws-org", "lin-me", "Me In Linear", "api_key"); err != nil {
		t.Fatalf("UpsertLinearIdentity(org): %v", err)
	}
	got := getMeLinear(t, s)
	if got.LinearUserID == nil || *got.LinearUserID != "lin-me" {
		t.Errorf("linear_user_id = %v, want lin-me", got.LinearUserID)
	}
	if got.LinearDisplayName == nil || *got.LinearDisplayName != "Me In Linear" {
		t.Errorf("linear_display_name = %v, want %q", got.LinearDisplayName, "Me In Linear")
	}
}
