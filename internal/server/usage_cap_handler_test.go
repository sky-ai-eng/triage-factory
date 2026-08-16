package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// putCapReq builds a PUT /api/usage/teams/{team_id}/cap carrying the session org,
// caller claims, the team_id path value, and a JSON body — the state
// withCSRF/withSession would normally seed. The handler is invoked directly (not
// via the mux) so the test exercises the governance + role gates and the
// admin-pool write without the cookie-session dance. Reuses the usageRig from
// usage_handler_gates_test.go (same package, same four actors + seeded spend).
func (r *usageRig) putCapReq(caller, teamID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/usage/teams/"+teamID+"/cap", strings.NewReader(body))
	req.SetPathValue("team_id", teamID)
	ctx := httpx.WithOrgID(req.Context(), r.orgID)
	ctx = httpx.WithClaims(ctx, &verify.Claims{Subject: caller})
	return req.WithContext(ctx)
}

type capEcho struct {
	MaxDailyCostUSD *float64 `json:"max_daily_cost_usd"`
}

// TestUsageTeamCap_GovernanceAndRoleGates_Postgres pins the per-team cap PUT
// (TFAC-482): unlicensed → 404 (the EE-feature-hidden posture), org-admin +
// governance → 200 and the value lands on the org rollup's by_team row, and
// team-admin / plain-member → 403. Plus the clear-with-null and negative-400
// edges. Multi-mode only (the gates short-circuit in local); skips without Docker.
func TestUsageTeamCap_GovernanceAndRoleGates_Postgres(t *testing.T) {
	r := newUsageRig(t)

	t.Run("unlicensed_404_even_for_org_admin", func(t *testing.T) {
		entitlements.Reset() // community default: governance OFF
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.orgAdmin, r.teamA, `{"max_daily_cost_usd": 25}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unlicensed cap PUT = %d, want 404 (feature hidden); body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_admin_with_governance_sets_cap_and_it_shows_on_rollup", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)

		// orgAdmin is an org admin but NOT a member of teamA — proving the
		// admin-pool write past the team-admin-gated RLS.
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.orgAdmin, r.teamA, `{"max_daily_cost_usd": 25}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("org-admin cap PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		// The echo is read back from the DB (not the request body), so it reflects
		// what actually landed.
		var echo capEcho
		mustDecode(t, rec, &echo)
		if echo.MaxDailyCostUSD == nil || !floatEq(*echo.MaxDailyCostUSD, 25) {
			t.Errorf("echoed cap = %v, want 25", echo.MaxDailyCostUSD)
		}

		// The cap landed: the editor's source (team-caps/list) reports it on teamA.
		capsRec := httptest.NewRecorder()
		r.uh.handleUsageTeamCaps(capsRec, r.activityReq(r.orgAdmin, "", ""))
		caps := decodeList[usageTeamCapEntry](t, capsRec)
		var sawTeamA bool
		for _, e := range caps.Items {
			if e.TeamID == r.teamA {
				sawTeamA = true
				if e.Cap == nil || !floatEq(*e.Cap, 25) {
					t.Errorf("team-caps teamA cap = %v, want 25", e.Cap)
				}
			}
		}
		if !sawTeamA {
			t.Errorf("teamA absent from team-caps after setting its cap; teams=%+v", caps.Items)
		}
		// Both seeded teams are listed, and the total counts every active team
		// — not only the capped ones. The editor's whole reason to exist is
		// pre-capping an idle team.
		if caps.Total() != 2 || len(caps.Items) != 2 {
			t.Errorf("team-caps = %d rows / total %d, want 2/2 (teamA + teamB)", len(caps.Items), caps.Total())
		}
	})

	t.Run("team_admin_403", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)
		// A teamA admin is still only an org *member* — per-team cap config is
		// org-admin-only (a team admin cannot set their own team's cap).
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.teamAdmin, r.teamA, `{"max_daily_cost_usd": 10}`))
		if rec.Code != http.StatusForbidden {
			t.Errorf("team-admin cap PUT = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("plain_member_403", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.member, r.teamA, `{"max_daily_cost_usd": 10}`))
		if rec.Code != http.StatusForbidden {
			t.Errorf("plain-member cap PUT = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("clear_with_null_stores_no_cap", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.orgAdmin, r.teamB, `{"max_daily_cost_usd": null}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("clear cap PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var echo capEcho
		mustDecode(t, rec, &echo)
		if echo.MaxDailyCostUSD != nil {
			t.Errorf("cleared cap echo = %v, want null", *echo.MaxDailyCostUSD)
		}
	})

	t.Run("negative_400", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.orgAdmin, r.teamA, `{"max_daily_cost_usd": -5}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("negative cap PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestUsageTeamCaps_ListsEveryActiveTeam_Postgres pins the team-caps editor
// source: it lists every ACTIVE team — including one with NO spend in the
// window, which the spend rollup's by_team would hide — so an org admin can
// pre-cap an idle team. Same governance/role gates as the PUT.
func TestUsageTeamCaps_ListsEveryActiveTeam_Postgres(t *testing.T) {
	r := newUsageRig(t)
	// A third team with NO seeded spend — the idle case the editor must surface.
	teamC := pgtest.SeedTeam(t, r.h, r.orgID, "teamC")

	t.Run("unlicensed_404", func(t *testing.T) {
		entitlements.Reset()
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCaps(rec, r.activityReq(r.orgAdmin, "", ""))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unlicensed team-caps = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin_403", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCaps(rec, r.activityReq(r.teamAdmin, "", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("team-admin team-caps = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("org_admin_lists_all_active_including_idle", func(t *testing.T) {
		entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
		t.Cleanup(entitlements.Reset)

		// Pre-cap the idle teamC, to prove the just-set value round-trips on the list.
		putRec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(putRec, r.putCapReq(r.orgAdmin, teamC, `{"max_daily_cost_usd": 30}`))
		if putRec.Code != http.StatusOK {
			t.Fatalf("PUT cap on idle teamC = %d, want 200; body=%s", putRec.Code, putRec.Body.String())
		}

		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCaps(rec, r.activityReq(r.orgAdmin, "", ""))
		resp := decodeList[usageTeamCapEntry](t, rec)
		if resp.Total() != 3 {
			t.Errorf("team-caps total = %d, want 3 (every active team)", resp.Total())
		}

		caps := map[string]*float64{}
		for _, e := range resp.Items {
			caps[e.TeamID] = e.Cap
		}
		// Every active team is present — including teamC, which has zero spend and
		// would be absent from the spend rollup's by_team.
		for _, id := range []string{r.teamA, r.teamB, teamC} {
			if _, ok := caps[id]; !ok {
				t.Errorf("team-caps missing team %s; want every active team", id)
			}
		}
		// teamC's just-set cap is reflected; teamA (no cap) reads null.
		if caps[teamC] == nil || !floatEq(*caps[teamC], 30) {
			t.Errorf("team-caps teamC cap = %v, want 30", caps[teamC])
		}
		if caps[r.teamA] != nil {
			t.Errorf("team-caps teamA cap = %v, want null (no cap set)", *caps[r.teamA])
		}
	})
}
