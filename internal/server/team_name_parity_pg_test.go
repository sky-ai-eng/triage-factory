package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// createReq builds a POST /api/teams as callerID, seeded with the same claims +
// active org the rename rig's req does. Both team-name doors are multi-only
// (404 in local), so the parity assertion lives on the Postgres rig.
func (r *teamRenameRig) createReq(callerID string, body any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/teams", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

// TestTeamName_SameRuleAtBothDoors pins the create/rename symmetry: a name one
// door refuses, the other refuses too, with the same reason and the same field.
// Create used to skip the length cap entirely, so a team could be created with
// a name its own rename would then reject — the shape of gap a per-door
// validator produces, and the reason both doors now call validateTeamName.
func TestTeamName_SameRuleAtBothDoors(t *testing.T) {
	r := newTeamRenameRig(t)

	for _, tc := range []struct {
		what   string
		name   string
		reason string
	}{
		{"over the length cap", strings.Repeat("x", maxTeamNameLen+1), httpx.ReasonOutOfRange},
		{"no slug-able characters", "!!!", httpx.ReasonInvalidField},
		{"blank", "   ", httpx.ReasonMissingField},
	} {
		t.Run(tc.what, func(t *testing.T) {
			create := httptest.NewRecorder()
			r.th.handleTeamCreate(create, r.createReq(r.admin, map[string]string{"name": tc.name}))
			if create.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400; body=%s", create.Code, create.Body.String())
			}
			assertFirstError(t, create, tc.reason, "name")

			rename := httptest.NewRecorder()
			r.th.handleTeamUpdate(rename, r.req(r.admin, r.teamID, map[string]string{"name": tc.name}))
			if rename.Code != http.StatusBadRequest {
				t.Fatalf("rename = %d, want 400; body=%s", rename.Code, rename.Body.String())
			}
			assertFirstError(t, rename, tc.reason, "name")

			if name, _, _ := r.nameSlugDescOf(t, r.teamID); name == tc.name {
				t.Errorf("team renamed to the refused name %q", tc.name)
			}
		})
	}
}
