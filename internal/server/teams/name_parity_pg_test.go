package teams

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// createReq builds a POST /api/orgs/{org_id}/teams for the rig's org as
// callerID, seeded with the same claims the rename rig's req does. Both team-name doors are multi-only
// (404 in local), so the parity assertion lives on the Postgres rig.
func (r *teamRenameRig) createReq(callerID string, body any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/orgs/"+r.orgID+"/teams", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("org_id", r.orgID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

// TestTeamName_SameRuleAtBothDoors: a name create refuses, rename refuses too,
// with the same reason and field, and the team keeps its name.
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
			httpxtest.AssertFirstError(t, create, tc.reason, "name")

			rename := httptest.NewRecorder()
			r.th.handleTeamUpdate(rename, r.req(r.admin, r.teamID, map[string]string{"name": tc.name}))
			if rename.Code != http.StatusBadRequest {
				t.Fatalf("rename = %d, want 400; body=%s", rename.Code, rename.Body.String())
			}
			httpxtest.AssertFirstError(t, rename, tc.reason, "name")

			if name, _, _ := r.nameSlugDescOf(t, r.teamID); name == tc.name {
				t.Errorf("team renamed to the refused name %q", tc.name)
			}
		})
	}
}

// TestTeamCreate_SlugMustBeInSlugForm: a slug the caller supplies is stored as
// sent or refused, never rewritten into a different one.
func TestTeamCreate_SlugMustBeInSlugForm(t *testing.T) {
	r := newTeamRenameRig(t)

	for _, slug := range []string{
		"Platform Eng",
		"PLATFORM",
		"platform--eng",
		"-platform",
		"platform_eng",
		strings.Repeat("x", domain.MaxSlugLen+1),
	} {
		t.Run(slug, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.th.handleTeamCreate(rec, r.createReq(r.admin, map[string]string{"name": "Platform", "slug": slug}))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidField, "slug")
		})
	}

	rec := httptest.NewRecorder()
	r.th.handleTeamCreate(rec, r.createReq(r.admin, map[string]string{"name": "Platform", "slug": "ops-2"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got teamJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "ops-2" {
		t.Errorf("slug = %q, want ops-2 as sent", got.Slug)
	}
}
