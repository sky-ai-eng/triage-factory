package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// TestUsageGovernanceGates_NoLicenceOracle_Postgres pins the property the whole
// role-before-entitlement ordering exists for: a caller who fails the role check
// gets the SAME status whether or not the deployment is licensed for governance.
//
// The failure it guards against is subtle and easy to reintroduce, because each
// gate is defensible alone. Check the entitlement first and an unlicensed
// deployment answers 404 to everyone, which reads like good hygiene — but then a
// plain member who probes the route sees 404 here and 403 on a licensed
// deployment, and the difference between those two answers is the org's plan
// tier. Nobody below org admin is supposed to be able to read that off the API.
//
// So this asserts sameness across the licence axis, not any particular code. The
// route's actual gate strength is covered by the per-route tests; what would slip
// past those is exactly a divergence here.
//
// Multi-mode only (both gates short-circuit in local); skips without Docker.
func TestUsageGovernanceGates_NoLicenceOracle_Postgres(t *testing.T) {
	r := newUsageRig(t)

	// Every governance-gated usage route, driven by a caller who must fail its
	// role check. member is neither an org admin nor an admin of teamA, so it
	// fails all of them; teamAdmin additionally fails the org-admin-only ones.
	routes := []struct {
		name   string
		caller string
		invoke func(w http.ResponseWriter, caller string)
	}{
		{"org_access_log", r.member, func(w http.ResponseWriter, c string) {
			r.uh.handleUsageAccessLog(w, r.req(c, ""))
		}},
		{"org_artifacts", r.member, func(w http.ResponseWriter, c string) {
			r.uh.handleUsageOrgArtifacts(w, r.activityReq(c, "", ""))
		}},
		{"org_actions", r.member, func(w http.ResponseWriter, c string) {
			r.uh.handleUsageOrgActions(w, r.activityReq(c, "", ""))
		}},
		{"org_team_caps", r.teamAdmin, func(w http.ResponseWriter, c string) {
			r.uh.handleUsageTeamCaps(w, r.activityReq(c, "", ""))
		}},
		{"team_cap_put", r.teamAdmin, func(w http.ResponseWriter, c string) {
			r.uh.handleUsageTeamCap(w, r.putCapReq(c, r.teamA, `{"max_daily_cost_usd": 25}`))
		}},
		{"team_artifacts", r.member, func(w http.ResponseWriter, c string) {
			r.uh.handleUsageTeamArtifacts(w, r.activityReq(c, r.teamA, ""))
		}},
		{"team_actions", r.member, func(w http.ResponseWriter, c string) {
			r.uh.handleUsageTeamActions(w, r.activityReq(c, r.teamA, ""))
		}},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			entitlements.Reset() // community default: governance OFF
			unlicensed := httptest.NewRecorder()
			rt.invoke(unlicensed, rt.caller)

			entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
			t.Cleanup(entitlements.Reset)
			licensed := httptest.NewRecorder()
			rt.invoke(licensed, rt.caller)

			if unlicensed.Code != licensed.Code {
				t.Fatalf("a non-admin got %d unlicensed and %d licensed — the status is a licence oracle; "+
					"run the role check before the entitlement", unlicensed.Code, licensed.Code)
			}
			if unlicensed.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 on both (the role refusal is what a non-admin should see); body=%s",
					unlicensed.Code, unlicensed.Body.String())
			}
		})
	}
}

// TestUsageGovernanceGates_AdminStillSees404Unlicensed is the other half: the
// entitlement still hides the feature from the caller who would otherwise be let
// in. Without this, "make the statuses match" could be satisfied by dropping the
// entitlement check altogether.
func TestUsageGovernanceGates_AdminStillSees404Unlicensed_Postgres(t *testing.T) {
	r := newUsageRig(t)
	entitlements.Reset()
	t.Cleanup(entitlements.Reset)

	for _, tc := range []struct {
		name   string
		invoke func(w http.ResponseWriter)
	}{
		{"org_access_log", func(w http.ResponseWriter) { r.uh.handleUsageAccessLog(w, r.req(r.orgAdmin, "")) }},
		{"org_artifacts", func(w http.ResponseWriter) {
			r.uh.handleUsageOrgArtifacts(w, r.activityReq(r.orgAdmin, "", ""))
		}},
		{"org_team_caps", func(w http.ResponseWriter) {
			r.uh.handleUsageTeamCaps(w, r.activityReq(r.orgAdmin, "", ""))
		}},
		{"team_artifacts", func(w http.ResponseWriter) {
			r.uh.handleUsageTeamArtifacts(w, r.activityReq(r.teamAdmin, r.teamA, ""))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.invoke(rec)
			if rec.Code != http.StatusNotFound {
				t.Errorf("unlicensed admin got %d, want 404 (the feature stays hidden); body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
