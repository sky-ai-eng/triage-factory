package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestBedrockRoleSetup_GatedOnThePathOrgsAdmin: the setup generates the org's
// External ID, so it answers the {org_id} it names, and only to that org's
// admins: a plain member is 403, and a caller outside the org is 404.
func TestBedrockRoleSetup_GatedOnThePathOrgsAdmin(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	s := New(h.AdminDB, pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey))
	orgID, owner, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamID, "member", "member")
	_, outsider, _ := pgtest.SeedOrgWithUser(t, h, "outsider")

	resolver := &fakeRoleResolver{arn: "arn:aws:iam::111122223333:role/tf-control"}
	se := &settingsHandler{tx: s.tx, az: s.az, bedrockRole: func() bedrockRoleResolver { return resolver }}
	setup := func(caller string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/orgs/"+orgID+"/llm/bedrock/role-setup", nil)
		req.SetPathValue("org_id", orgID)
		req = req.WithContext(httpx.WithClaims(req.Context(), &verify.Claims{Subject: caller}))
		rec := httptest.NewRecorder()
		se.handleBedrockRoleSetup(rec, req)
		return rec
	}

	for _, c := range []struct {
		name   string
		caller string
		want   int
	}{
		{"member", member, http.StatusForbidden},
		{"outsider", outsider, http.StatusNotFound},
		{"owner", owner, http.StatusOK},
	} {
		if rec := setup(c.caller); rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}
