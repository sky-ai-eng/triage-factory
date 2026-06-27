package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
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
		entitlements.Register(governanceGrant{})
		t.Cleanup(entitlements.Reset)

		// orgAdmin is an org admin but NOT a member of teamA — proving the
		// admin-pool write past the team-admin-gated RLS.
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.orgAdmin, r.teamA, `{"max_daily_cost_usd": 25}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("org-admin cap PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var echo capEcho
		mustDecode(t, rec, &echo)
		if echo.MaxDailyCostUSD == nil || !floatEq(*echo.MaxDailyCostUSD, 25) {
			t.Errorf("echoed cap = %v, want 25", echo.MaxDailyCostUSD)
		}

		// The org rollup now reports the cap on teamA's by_team row (spend + cap
		// together), via the admin-pool resolveSpendTeamCaps read.
		orgRec := httptest.NewRecorder()
		r.uh.handleUsageOrg(orgRec, r.req(r.orgAdmin, ""))
		if orgRec.Code != http.StatusOK {
			t.Fatalf("/org after cap set = %d, want 200; body=%s", orgRec.Code, orgRec.Body.String())
		}
		var org usageOrgResponse
		mustDecode(t, orgRec, &org)
		var sawTeamA bool
		for _, b := range org.ByTeam {
			if b.TeamID == r.teamA {
				sawTeamA = true
				if b.Cap == nil || !floatEq(*b.Cap, 25) {
					t.Errorf("by_team[teamA].cap = %v, want 25", b.Cap)
				}
			}
		}
		if !sawTeamA {
			t.Errorf("teamA absent from by_team after setting its cap; by_team=%+v", org.ByTeam)
		}
	})

	t.Run("team_admin_403", func(t *testing.T) {
		entitlements.Register(governanceGrant{})
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
		entitlements.Register(governanceGrant{})
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.member, r.teamA, `{"max_daily_cost_usd": 10}`))
		if rec.Code != http.StatusForbidden {
			t.Errorf("plain-member cap PUT = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("clear_with_null_stores_no_cap", func(t *testing.T) {
		entitlements.Register(governanceGrant{})
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
		entitlements.Register(governanceGrant{})
		t.Cleanup(entitlements.Reset)
		rec := httptest.NewRecorder()
		r.uh.handleUsageTeamCap(rec, r.putCapReq(r.orgAdmin, r.teamA, `{"max_daily_cost_usd": -5}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("negative cap PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}
