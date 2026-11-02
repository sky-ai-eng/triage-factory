package teams

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// orgListReq builds POST /api/orgs/{orgID}/teams/list as callerID.
func orgListReq(callerID, orgID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/orgs/"+orgID+"/teams/list", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("org_id", orgID)
	return req.WithContext(httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID}))
}

// TestOrgTeamsList_Postgres: every member of the org reads every active team
// in it under RLS, each row carrying the caller's own role, none on a team
// they are not on; archived teams are left out, and a caller outside the org
// finds nothing.
func TestOrgTeamsList_Postgres(t *testing.T) {
	r := newTeamRenameRig(t)
	second := pgtest.SeedTeam(t, r.h, r.orgID, "second")
	archived := pgtest.SeedTeam(t, r.h, r.orgID, "archived")
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, archived)
	_, outsider, _ := pgtest.SeedOrgWithUser(t, r.h, "elsewhere")

	for _, c := range []struct {
		who                  string
		caller               string
		roleFirst, roleOther string
	}{
		{"a member of the first team", r.memb, "member", ""},
		{"an org admin on neither", r.orgAdmin, "", ""},
	} {
		t.Run(c.who, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.th.handleOrgTeamsList(rec, orgListReq(c.caller, r.orgID))
			page := httpxtest.DecodeList[teamJSON](t, rec)
			if page.Total() != 2 || len(page.Items) != 2 {
				t.Fatalf("teams = %+v (total %d); want the two active teams", page.Items, page.Total())
			}
			roles := map[string]string{}
			for _, tm := range page.Items {
				roles[tm.ID] = tm.Role
			}
			if got, ok := roles[r.teamID]; !ok || got != c.roleFirst {
				t.Errorf("first team role = %q (listed %v); want %q", got, ok, c.roleFirst)
			}
			if got, ok := roles[second]; !ok || got != c.roleOther {
				t.Errorf("second team role = %q (listed %v); want %q", got, ok, c.roleOther)
			}
		})
	}

	rec := httptest.NewRecorder()
	r.th.handleOrgTeamsList(rec, orgListReq(outsider, r.orgID))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a caller outside the org = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
